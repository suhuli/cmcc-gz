"""路径 -> fileId 解析与目录条目缓存。"""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass, field
from typing import Any

from .api import MCloudClient

ROOT_FILE_ID = "/"
DIR_TTL = 15.0
NEGATIVE_TTL = 3.0


@dataclass
class Entry:
    name: str
    file_id: str
    parent_file_id: str
    is_dir: bool
    size: int = 0
    updated_at: float | None = None
    created_at: float | None = None
    etag: str = ""


@dataclass
class DirCache:
    fetched_at: float
    entries: dict[str, Entry] = field(default_factory=dict)


class Vfs:
    """把 WebDAV 路径映射为 fileId，并缓存目录页。"""

    def __init__(self, client: MCloudClient):
        self.client = client
        self._lock = threading.RLock()
        self._dir_cache: dict[str, DirCache] = {}
        self._root_path = "/"

    def invalidate(self, parent_file_id: str | None = None) -> None:
        with self._lock:
            if parent_file_id is None:
                self._dir_cache.clear()
            else:
                self._dir_cache.pop(parent_file_id, None)

    def _fetch_children(self, parent_file_id: str) -> DirCache:
        now = time.time()
        cache = DirCache(now, {})
        for item in self.client.iter_folder_items(parent_file_id):
            entry = Entry(
                name=str(item.get("name") or ""),
                file_id=str(item.get("fileId") or ""),
                parent_file_id=parent_file_id,
                is_dir=self._item_is_dir(item),
                size=int(item.get("size") or 0),
                updated_at=self._parse_time(item.get("updatedAt")),
                created_at=self._parse_time(item.get("createdAt")),
                etag=str(item.get("contentHash") or item.get("fileId") or ""),
            )
            if entry.name and entry.file_id:
                cache.entries[entry.name] = entry
        return cache
    def children(self, parent_file_id: str) -> list[Entry]:
        with self._lock:
            cached = self._dir_cache.get(parent_file_id)
            if cached and (time.time() - cached.fetched_at) < DIR_TTL:
                return list(cached.entries.values())
        try:
            fresh = self._fetch_children(parent_file_id)
        except Exception:
            if cached:
                return list(cached.entries.values())
            raise
        with self._lock:
            self._dir_cache[parent_file_id] = fresh
            return list(fresh.entries.values())

    def child(self, parent_file_id: str, name: str) -> Entry | None:
        for entry in self.children(parent_file_id):
            if entry.name == name:
                return entry
        return None

    def entry_at(self, path: str) -> Entry | None:
        parts = self.parts(path)
        if not parts:
            return None
        parent_file_id = ROOT_FILE_ID
        entry: Entry | None = None
        for part in parts:
            entry = self.child(parent_file_id, part)
            if entry is None:
                return None
            parent_file_id = entry.file_id
        return entry

    def parent_entry_at(self, path: str) -> tuple[str, str | None]:
        """返回 (parent_file_id, name)。"""
        parts = self.parts(path)
        if not parts:
            return ROOT_FILE_ID, None
        parent_file_id = ROOT_FILE_ID
        for part in parts[:-1]:
            entry = self.child(parent_file_id, part)
            if entry is None:
                return ROOT_FILE_ID, None
            parent_file_id = entry.file_id
        return parent_file_id, parts[-1]

    def parts(self, path: str) -> list[str]:
        return [part for part in path.strip("/").split("/") if part]

    def ensure_dir(self, parent_file_id: str, name: str) -> Entry:
        existing = self.child(parent_file_id, name)
        if existing:
            if not existing.is_dir:
                raise FileExistsError(name)
            return existing
        result = self.client.create_folder(parent_file_id, name)
        data = result.get("data") or {}
        entry = Entry(
            name=str(data.get("name") or data.get("fileName") or name),
            file_id=str(data.get("fileId") or ""),
            parent_file_id=parent_file_id,
            is_dir=True,
        )
        self.invalidate(parent_file_id)
        return entry

    @staticmethod
    def _item_is_dir(item: dict[str, Any]) -> bool:
        if str(item.get("type") or "").lower() == "folder":
            return True
        if item.get("systemDir") is True:
            return True
        return str(item.get("type") or "").lower() == "dir"

    @staticmethod
    def _parse_time(value: Any) -> float | None:
        if value in (None, ""):
            return None
        if isinstance(value, (int, float)):
            seconds = float(value)
            if seconds > 1e12:
                seconds /= 1000.0
            return seconds
        try:
            from datetime import datetime

            text = str(value).replace("Z", "+00:00")
            dt = datetime.fromisoformat(text)
            if dt.tzinfo is None:
                return dt.timestamp()
            return dt.timestamp()
        except Exception:
            return None
