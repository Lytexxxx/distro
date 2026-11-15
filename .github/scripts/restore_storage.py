import mimetypes
import os
import pathlib
import urllib.error
import urllib.parse
import urllib.request
import json

base_url = os.environ["SUPABASE_URL"].rstrip("/")
service_key = os.environ["SUPABASE_SERVICE_ROLE_KEY"]
bucket = os.environ.get("SUPABASE_STORAGE_BUCKET", "distro-files")
source = pathlib.Path("backups/storage")

headers = {
    "apikey": service_key,
    "Authorization": f"Bearer {service_key}",
    "Content-Type": "application/json",
}


def request(req, timeout=60):
    try:
        return urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as error:
        body = error.read().decode("utf-8", errors="replace")
        # A bucket that already exists is acceptable for a repeat test.
        if req.full_url.endswith("/storage/v1/bucket") and error.code == 409:
            return None
        raise RuntimeError(
            f"Supabase Storage returned HTTP {error.code}: {body[:1000]}"
        ) from None


# Create the private bucket if it does not exist yet.
bucket_url = f"{base_url}/storage/v1/bucket"
payload = json.dumps({"id": bucket, "name": bucket, "public": False}).encode("utf-8")
req = urllib.request.Request(bucket_url, data=payload, headers=headers, method="POST")
response = request(req)
if response is not None:
    response.close()

if not source.exists():
    raise RuntimeError("Le dossier de sauvegarde Storage est introuvable.")

uploaded = 0
total_bytes = 0
for path in sorted(source.rglob("*")):
    if not path.is_file():
        continue
    relative = path.relative_to(source)
    if relative.name in {"BACKUP_INFO.txt", "EMPTY_BUCKET.txt"}:
        continue
    object_path = relative.as_posix()
    encoded_path = urllib.parse.quote(object_path, safe="/")
    url = (
        f"{base_url}/storage/v1/object/"
        f"{urllib.parse.quote(bucket, safe='')}/{encoded_path}"
    )
    content_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
    data = path.read_bytes()
    upload_headers = {
        "apikey": service_key,
        "Authorization": f"Bearer {service_key}",
        "Content-Type": content_type,
        "x-upsert": "true",
    }
    req = urllib.request.Request(url, data=data, headers=upload_headers, method="POST")
    with request(req, timeout=120) as response:
        if response.status < 200 or response.status >= 300:
            raise RuntimeError(f"Échec de restauration de l'objet {object_path}")
    uploaded += 1
    total_bytes += len(data)
    print(f"Restored {object_path} ({len(data)} bytes)")

print(f"Storage restore completed: {uploaded} objects, {total_bytes} bytes.")
