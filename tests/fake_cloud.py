"""内存版云盘假客户端，用于离线测试 WebDAV 适配层。

支持：目录、分片上传、移动、重命名、回收站、Range 下载，
以及故障注入（下载状态码、重命名失败、忽略 Range）。
"""

from __future__ import annotations

import io
import itertools
import re
from typing import Any

from mcloudmount.config import Account, Config
from mcloudmount.errors import ApiError, ConflictError, NotFoundError

ROOT = "/"


class FakeResponse:
    def __init__(self, data: bytes = b"", status: int = 200, headers: dict[str, str] | None = None):
        self.status_code = status
        self.headers = {"ETag": "fake", **(headers or {})}
        self.raw = io.BytesIO(data)
        self.content = data
        self.text = data.decode("utf-8", "replace")
        self.ok = status < 400

    def close(self) -> None:
        self.raw.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()


class FakeCloud:
    def __init__(self):
        self.config = Config(account=Account(phone="13000000000", token="fake", account="13000000000"))
        self.nodes: dict[str, dict[str, Any]] = {}
        self._ids = itertools.count(1)
        self.calls: dict[str, int] = {}
        # 故障注入
        self.download_status: int | None = None  # 指定后所有下载返回该状态码
        self.ignore_range = False  # True 时忽略 Range，返回 200 + 全量
        self.fail_new_rename = False  # True 时：把临时上传文件改为目标名会失败
        self.fail_move = False

    # ------------------------------------------------------------ 工具
    def _count(self, name: str) -> None:
        self.calls[name] = self.calls.get(name, 0) + 1

    def _children(self, parent: str) -> list[dict[str, Any]]:
        return [n for n in self.nodes.values() if n["parent"] == parent and not n.get("trashed")]

    def _new_node(self, parent: str, name: str, is_dir: bool) -> dict[str, Any]:
        file_id = str(next(self._ids))
        node = {
            "fileId": file_id,
            "name": name,
            "parent": parent,
            "type": "folder" if is_dir else "file",
            "size": 0,
            "content": b"",
            "pending": {},
            "trashed": False,
        }
        self.nodes[file_id] = node
        return node

    def _exists(self, parent: str, name: str) -> bool:
        return any(n["name"] == name for n in self._children(parent))

    def _node_out(self, node: dict[str, Any]) -> dict[str, Any]:
        return {
            "fileId": node["fileId"],
            "name": node["name"],
            "type": node["type"],
            "size": node["size"],
        }

    # ------------------------------------------------------------ 目录
    def iter_folder_items(self, parent_file_id: str = ROOT, **_: Any):
        self._count("list")
        return [self._node_out(n) for n in self._children(parent_file_id)]

    def create_folder(self, parent_file_id: str, name: str, **_: Any):
        self._count("mkdir")
        if self._exists(parent_file_id, name):
            raise ConflictError("exists", code="409")
        node = self._new_node(parent_file_id, name, True)
        return {"data": self._node_out(node)}

    # ------------------------------------------------------------ 上传
    def upload_create(self, parent_file_id: str, name: str, size: int, **kwargs: Any):
        self._count("upload_create")
        if self._exists(parent_file_id, name) and kwargs.get("file_rename_mode") == "refuse":
            raise ConflictError("exists", code="409")
        node = self._new_node(parent_file_id, name, False)
        node["size"] = size
        node["upload_id"] = f"up-{node['fileId']}"
        parts = kwargs.get("part_infos") or []
        return {
            "data": {
                "fileId": node["fileId"],
                "uploadId": node["upload_id"],
                "transferToken": f"tok-{node['fileId']}",
                "partInfos": [
                    {"partNumber": p["partNumber"], "uploadUrl": f"fake://{node['fileId']}/{p['partNumber']}"}
                    for p in parts
                ],
            }
        }

    def upload_part(self, upload_url: str, data: Any, length: int | None = None, **_: Any):
        self._count("upload_part")
        match = re.fullmatch(r"fake://(\d+)/(\d+)", upload_url)
        if not match:
            raise ApiError("bad url", http_status=400)
        file_id, number = match.group(1), int(match.group(2))
        payload = data.read(length) if length is not None else data.read()
        self.nodes[file_id]["pending"][number] = payload
        return FakeResponse(b"", 200, {"etag": f"\"part-{number}\""})

    def complete_upload(self, file_id: str, upload_id: str = "", transfer_token: str = "", content_hash: str = "", **_: Any):
        self._count("complete")
        node = self.nodes[file_id]
        node["content"] = b"".join(node["pending"][k] for k in sorted(node["pending"]))
        node["size"] = len(node["content"])
        node["pending"] = {}
        return {"data": {"fileId": file_id}}

    # ------------------------------------------------------------ 变更
    def rename(self, file_id: str, new_name: str, **_: Any):
        self._count("rename")
        node = self.nodes.get(file_id)
        if node is None or node.get("trashed"):
            raise NotFoundError("missing", code="404")
        if self.fail_new_rename and new_name and node["name"].startswith("__mcm_tmp_") and not new_name.startswith("__mcm"):
            raise ApiError("injected rename failure", code="500")
        if self._exists(node["parent"], new_name) and self.nodes.get(file_id, {}).get("name") != new_name:
            raise ConflictError("exists", code="409")
        node["name"] = new_name
        return {"data": self._node_out(node)}

    def move(self, file_ids: list[str], to_parent_file_id: str, **_: Any):
        self._count("move")
        if self.fail_move:
            raise ApiError("injected move failure", code="500")
        for file_id in file_ids:
            self.nodes[file_id]["parent"] = to_parent_file_id
        return {"data": {}}

    def trash(self, file_ids: list[str], **_: Any):
        self._count("trash")
        for file_id in file_ids:
            if not file_id:
                raise NotFoundError("empty id", code="404")
            node = self.nodes.get(file_id)
            if node:
                node["trashed"] = True
        return {"data": {}}

    # ------------------------------------------------------------ 下载
    def get_download_url(self, file_id: str, expire_sec: int = 3600):
        self._count("download_url")
        return {"data": {"url": f"fake-download://{file_id}"}}

    def download_stream(self, url: str, headers: dict[str, str] | None = None, **_: Any):
        self._count("download")
        if self.download_status is not None:
            return FakeResponse(b"", self.download_status)
        file_id = url.removeprefix("fake-download://")
        content = self.nodes[file_id]["content"]
        rng = (headers or {}).get("Range")
        if rng and not self.ignore_range:
            m = re.fullmatch(r"bytes=(\d+)-(\d*)", rng)
            start = int(m.group(1))
            end = int(m.group(2)) if m.group(2) else len(content) - 1
            return FakeResponse(content[start : end + 1], 206)
        return FakeResponse(content, 200)

    # ------------------------------------------------------------ 查询
    def content_of(self, parent: str, name: str) -> bytes | None:
        for node in self._children(parent):
            if node["name"] == name:
                return node["content"]
        return None

    def names_in(self, parent: str) -> list[str]:
        return sorted(n["name"] for n in self._children(parent))
