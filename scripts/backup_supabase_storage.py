#!/usr/bin/env python3
"""Download every object from a Supabase Storage bucket for backup."""

import os
import pathlib
import sys
import time
from urllib.parse import quote

import requests

SUPABASE_URL = os.environ["SUPABASE_URL"].rstrip("/")
SERVICE_ROLE_KEY = os.environ["SUPABASE_SERVICE_ROLE_KEY"]
BUCKET = os.environ.get("SUPABASE_STORAGE_BUCKET", "distro-files")
DESTINATION = pathlib.Path(os.environ.get("BACKUP_DIR", "storage-backup"))


def headers():
    return {
        "apikey": SERVICE_ROLE_KEY,
        "Authorization": f"Bearer {SERVICE_ROLE_KEY}",
    }


def list_folder(prefix: str):
    url = f"{SUPABASE_URL}/storage/v1/object/list/{quote(BUCKET, safe='')}"
    all_entries = []
    offset = 0
    while True:
        response = requests.post(
            url,
            headers={**headers(), "Content-Type": "application/json"},
            json={
                "prefix": prefix,
                "limit": 1000,
                "offset": offset,
                "sortBy": {"column": "name", "order": "asc"},
            },
            timeout=60,
        )
        response.raise_for_status()
        entries = response.json()
        if not isinstance(entries, list):
            raise RuntimeError(f"Réponse inattendue du listing Storage: {entries}")
        all_entries.extend(entries)
        if len(entries) < 1000:
            break
        offset += len(entries)
    return all_entries


def download_object(object_path: str):
    url = (
        f"{SUPABASE_URL}/storage/v1/object/"
        f"{quote(BUCKET, safe='')}/{quote(object_path, safe='/')}"
    )
    response = requests.get(url, headers=headers(), timeout=180)
    response.raise_for_status()
    target = DESTINATION.joinpath(*object_path.split("/"))
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(response.content)
    print(f"Téléchargé: {object_path} ({len(response.content)} octets)")


def walk(prefix: str, visited: set[str]):
    if prefix in visited:
        return
    visited.add(prefix)
    entries = list_folder(prefix)
    for entry in entries:
        name = entry.get("name")
        if not name:
            continue
        path = f"{prefix}/{name}" if prefix else name
        # Supabase Storage represents folders as entries without an object id.
        if entry.get("id") is None and entry.get("metadata") is None:
            walk(path, visited)
        else:
            download_object(path)


def main():
    DESTINATION.mkdir(parents=True, exist_ok=True)
    walk("", set())
    files = [p for p in DESTINATION.rglob("*") if p.is_file()]
    print(f"Sauvegarde Storage terminée: {len(files)} fichier(s).")
    if not files:
        print("Le bucket est vide (aucun objet à sauvegarder).")


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(f"ÉCHEC sauvegarde Storage: {exc}", file=sys.stderr)
        raise
