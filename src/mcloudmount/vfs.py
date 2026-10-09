"""路径 -> fileId 解析与目录条目缓存。

缓存结构：每个目录一份 DirCache，内含
- entries：精确名字 -> Entry（O(1) 查找）
- folded：casefold 名字 -> Entry（Windows 不区分大小写时的回退索引）
"""

from __future__ import annotations

import logging
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any

from .api import MCloudClient

log = logging.getLogger(__name__)

ROOT_FILE_ID = "/"
DIR_TTL = 15.0


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
    folded: dict[str, Entry] = field(default_factory=dict)

    def add(self, entry: Entry) -> None:
        self.entries[entry.name] = entry
        self.folded[entry.name.casefold()] = entry

    def remove(self, name: str) -> None:
        entry = self.entries.pop(name, None)
        if entry is not None:
            self.folded.pop(name.casefold(), None)

    def lookup(self, name: str) -> Entry | None:
        hit = self.entries.get(name)
        if hit is not None:
            return hit
        return self.folded.get(name.casefold())


class Vfs:
    """把 WebDAV 路径映射为 fileId，并缓存目录页。"""

    def __init__(self, client: MCloudClient, ttl: float = DIR_TTL):
        self.client = client
        self.ttl = ttl
        self._lock = threading.RLock()
        self._dir_cache: dict[str, DirCache] = {}

    # ------------------------------------------------------------ 缓存管理
    def invalidate(self, parent_file_id: str | None = None) -> None:
        with self._lock:
            if parent_file_id is None:
                self._dir_cache.clear()
            else:
                self._dir_cache.pop(parent_file_id, None)

    def remember(self, entry: Entry) -> None:
        """写操作成功后增量写入缓存，避免整目录重新拉取。"""
        with self._lock:
            cache = self._dir_cache.get(entry.parent_file_id)
            if cache is not None and entry.name:
                cache.add(entry)

    def forget(self, parent_file_id: str, name: str) -> None:
        with self._lock:
            cache = self._dir_cache.get(parent_file_id)
            if cache is not None:
                cache.remove(name)

    def _fetch_children(self, parent_file_id: str) -> DirCache:
        cache = DirCache(time.time())
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
                cache.add(entry)
        return cache

    def _cache_for(self, parent_file_id: str) -> DirCache:
        with self._lock:
            cached = self._dir_cache.get(parent_file_id)
            if cached and (time.time() - cached.fetched_at) < self.ttl:
                return cached
        try:
            fresh = self._fetch_children(parent_file_id)
        except Exception as exc:
            with self._lock:
                stale = self._dir_cache.get(parent_file_id)
            if stale is not None:
                # 网络失败时退回到过期缓存，并记录，避免静默吞错
                log.warning("目录刷新失败，使用过期缓存 %s: %s", parent_file_id, exc)
                return stale
            raise
        with self._lock:
            self._dir_cache[parent_file_id] = fresh
        return fresh

    # ------------------------------------------------------------ 查询
    def children(self, parent_file_id: str) -> list[Entry]:
        return list(self._cache_for(parent_file_id).entries.values())

    def child(self, parent_file_id: str, name: str) -> Entry | None:
        return self._cache_for(parent_file_id).lookup(name)

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
        """返回 (parent_file_id, name)。父路径不存在时 parent 为 None。"""
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

    @staticmethod
    def parts(path: str) -> list[str]:
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
        if entry.file_id:
            self.remember(entry)
        else:
            self.invalidate(parent_file_id)
        return entry

    # ------------------------------------------------------------ 解析工具
    @staticmethod
    def _item_is_dir(item: dict[str, Any]) -> bool:
        kind = str(item.get("type") or "").lower()
        if kind in ("folder", "dir"):
            return True
        return item.get("systemDir") is True

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
            text = str(value).replace("Z", "+00:00")
            return datetime.fromisoformat(text).timestamp()
        except ValueError:
            return None
