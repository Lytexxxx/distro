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

def request_json(url, payload):
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, headers=headers, method="POST")
    with urllib.request.urlopen(req, timeout=60) as response:
        return json.load(response)

def walk(prefix=""):
    offset = 0
    while True:
        url = f"{base_url}/storage/v1/object/list/{urllib.parse.quote(bucket, safe='')}"
        items = request_json(url, {
            "prefix": prefix,
            "limit": 1000,
            "offset": offset,
            "sortBy": {"column": "name", "order": "asc"},
        })
        if not items:
            break
        for item in items:
            name = item["name"]
            object_path = f"{prefix}/{name}" if prefix else name
            if item.get("id") is None and item.get("metadata") is None:
                walk(object_path)
                continue

            encoded_path = urllib.parse.quote(object_path, safe="/")
            download_url = f"{base_url}/storage/v1/object/{urllib.parse.quote(bucket, safe='')}/{encoded_path}"
            req = urllib.request.Request(download_url, headers={
                "apikey": service_key,
                "Authorization": f"Bearer {service_key}",
            })
            with urllib.request.urlopen(req, timeout=120) as response:
                body = response.read()

            destination = output / pathlib.PurePosixPath(object_path)
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(body)
            print(f"Saved {object_path} ({len(body)} bytes)")
        if len(items) < 1000:
            break
        offset += len(items)

walk()
print(f"Storage backup completed: {output}")
