import json
import os
import pathlib
import urllib.error
import urllib.parse
import urllib.request

base_url = os.environ["SUPABASE_URL"].rstrip("/")
service_key = os.environ["SUPABASE_SERVICE_ROLE_KEY"]
bucket = os.environ.get("SUPABASE_STORAGE_BUCKET", "distro-files")
output = pathlib.Path("backups/storage")
output.mkdir(parents=True, exist_ok=True)

headers = {
    "apikey": service_key,
    "Authorization": f"Bearer {service_key}",
    "Content-Type": "application/json",
}


def open_request(request, timeout):
    try:
        return urllib.request.urlopen(request, timeout=timeout)
    except urllib.error.HTTPError as error:
        # Do not print request headers or secrets.
        body = error.read().decode("utf-8", errors="replace")
        raise RuntimeError(
            f"Supabase Storage returned HTTP {error.code}: {body[:1000]}"
        ) from None


def request_json(url, payload):
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=headers, method="POST")
    with open_request(req, 60) as response:
        return json.load(response)


def safe_object_path(value):
    path = pathlib.PurePosixPath(value)
    if path.is_absolute() or not path.parts or any(part in ("", ".", "..") for part in path.parts):
        raise RuntimeError(f"Unsafe object path returned by Storage: {value!r}")
    if "\\\\" in value:
        raise RuntimeError(f"Unsafe object path returned by Storage: {value!r}")
    return path


downloaded = 0
total_bytes = 0


def walk(prefix=""):
    global downloaded, total_bytes
    offset = 0
    while True:
        url = (
            f"{base_url}/storage/v1/object/list/"
            f"{urllib.parse.quote(bucket, safe='')}"
        )
        items = request_json(url, {
            "prefix": prefix,
            "limit": 1000,
            "offset": offset,
            "sortBy": {"column": "name", "order": "asc"},
        })
        if not isinstance(items, list):
            raise RuntimeError(f"Unexpected Storage listing response for prefix {prefix!r}")

        if not items and not prefix and downloaded == 0:
            (output / "EMPTY_BUCKET.txt").write_text(
                f"The Supabase Storage bucket {bucket!r} was empty at backup time.\\n",
                encoding="utf-8",
            )

        for item in items:
            name = item.get("name")
            if not isinstance(name, str) or not name:
                raise RuntimeError(f"Invalid object entry returned for prefix {prefix!r}")
            object_path = f"{prefix}/{name}" if prefix else name
            relative_path = safe_object_path(object_path)

            # Supabase represents folders as entries without an object id or metadata.
            if item.get("id") is None and item.get("metadata") is None:
                walk(object_path)
                continue

            encoded_path = urllib.parse.quote(object_path, safe="/")
            download_url = (
                f"{base_url}/storage/v1/object/"
                f"{urllib.parse.quote(bucket, safe='')}/{encoded_path}"
            )
            req = urllib.request.Request(download_url, headers={
                "apikey": service_key,
                "Authorization": f"Bearer {service_key}",
            })
            with open_request(req, 120) as response:
                body = response.read()

            destination = output.joinpath(*relative_path.parts)
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(body)
            downloaded += 1
            total_bytes += len(body)
            print(f"Saved {object_path} ({len(body)} bytes)")

        if len(items) < 1000:
            break
        offset += len(items)


walk()
if downloaded:
    (output / "BACKUP_INFO.txt").write_text(
        f"Bucket: {bucket}\\nObjects: {downloaded}\\nBytes: {total_bytes}\\n",
        encoding="utf-8",
    )
print(f"Storage backup completed: {downloaded} objects, {total_bytes} bytes.")
