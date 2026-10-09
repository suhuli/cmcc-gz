"""把中国移动云盘映射为 WebDAV 目录。"""

from __future__ import annotations

import hashlib
import io
import tempfile
import threading
import time
import uuid
from typing import Any

from wsgidav.dav_error import (
    DAVError,
    HTTP_FORBIDDEN,
    HTTP_INTERNAL_ERROR,
    HTTP_NOT_FOUND,
)
from wsgidav.dav_provider import DAVCollection, DAVNonCollection, DAVProvider

from .api import MCloudClient
from .errors import MCloudError, NotFoundError
from .transport import default_http_headers
from .vfs import ROOT_FILE_ID, Entry, Vfs


UPLOAD_PART_SIZE = 8 * 1024 * 1024
DOWNLOAD_CHUNK_SIZE = 1024 * 1024


class _RangeStream:
    """Read cloud files through ranged requests, seeking by reopening the URL."""

    def __init__(self, client: MCloudClient, file_id: str, size: int = -1):
        self.client = client
        self.file_id = file_id
        self.size = size
        self.position = 0
        self._response = None
        self._url = ""

    def readable(self):
        return True

    def seekable(self):
        return True

    def _close_response(self):
        if self._response is not None:
            self._response.close()
            self._response = None

    def _open(self):
        if not self._url:
            result = self.client.get_download_url(self.file_id)
            self._url = str((result.get("data") or {}).get("url") or "")
        if not self._url:
            raise DAVError(HTTP_NOT_FOUND)
        headers = default_http_headers()
        if self.position:
            headers["Range"] = f"bytes={self.position}-"
        self._response = self.client.download_stream(self._url, headers=headers)
        if self.position and self._response.status_code != 206:
            raise DAVError(HTTP_INTERNAL_ERROR)
        if self._response.status_code >= 400:
            raise DAVError(HTTP_NOT_FOUND)

    def seek(self, offset: int, whence: int = 0):
        if whence == 0:
            target = offset
        elif whence == 1:
            target = self.position + offset
        elif whence == 2:
            if self.size < 0:
                raise OSError("cannot seek from end without size")
            target = self.size + offset
        else:
            raise ValueError(f"invalid whence: {whence}")
        target = max(0, target)
        if target != self.position:
            self._close_response()
            self.position = target
        return self.position

    def tell(self):
        return self.position

    def read(self, size=-1):
        if self.size >= 0 and self.position >= self.size:
            return b""
        if self._response is None:
            self._open()
        data = self._response.raw.read(size)
        if data:
            self.position += len(data)
        else:
            self._close_response()
        return data

    def close(self):
        self._close_response()


class _BoundedReader:
    """File-like view over a byte range, avoiding copies of large parts."""

    def __init__(self, source: tempfile.SpooledTemporaryFile, length: int):
        self.source = source
        self.remaining = max(0, int(length))

    def read(self, size: int = -1) -> bytes:
        if self.remaining <= 0:
            return b""
        if size is None or size < 0:
            size = self.remaining
        count = min(int(size), self.remaining)
        data = self.source.read(count)
        self.remaining -= len(data)
        return data


class _PutBuffer:
    """Keep PUT bytes readable after WsgiDAV closes the returned stream."""

    def __init__(self, source: tempfile.SpooledTemporaryFile):
        self.source = source
        self.closed = False

    def write(self, data: bytes) -> int:
        return self.source.write(data)

    def writelines(self, chunks) -> None:
        for chunk in chunks:
            self.write(chunk)

    def close(self) -> None:
        self.closed = True


class CloudFolder(DAVCollection):
    def __init__(self, path: str, environ: dict, provider: "CloudProvider", file_id: str):
        super().__init__(path, environ)
        self.provider_impl = provider
        self.file_id = file_id

    def get_member_names(self) -> list[str]:
        return [entry.name for entry in self.provider_impl.vfs.children(self.file_id)]

    def create_collection(self, name: str):
        self.provider_impl.vfs.ensure_dir(self.file_id, name)

    def create_empty_resource(self, name: str):
        entry = self.provider_impl.ensure_file_target(self.file_id, name)
        return CloudFile(join_path(self.path, name), self.environ, self.provider_impl, entry)

    def delete(self):
        if self.path == "/":
            raise DAVError(HTTP_FORBIDDEN)
        parent_file_id, _ = self.provider_impl.vfs.parent_entry_at(self.path)
        self.provider_impl.vfs.client.trash([self.file_id])
        self.provider_impl.vfs.invalidate(parent_file_id)

    def copy_move_single(self, dest_path: str, *, is_move: bool):
        dest_parent, name = self.provider_impl.vfs.parent_entry_at(dest_path)
        if not name:
            raise DAVError(HTTP_FORBIDDEN)
        self.provider_impl.vfs.ensure_dir(dest_parent, name)

    def support_recursive_delete(self) -> bool:
        return True

    def support_recursive_move(self, dest_path: str) -> bool:
        return True

    def move_recursive(self, dest_path: str):
        dest_parent, name = self.provider_impl.vfs.parent_entry_at(dest_path)
        if not name:
            raise DAVError(HTTP_FORBIDDEN)
        source_parent, source_name = self.provider_impl.vfs.parent_entry_at(self.path)
        self.provider_impl.vfs.client.move([self.file_id], dest_parent)
        if not self.provider_impl.wait_for_entry(dest_parent, source_name):
            raise DAVError(HTTP_INTERNAL_ERROR)
        if name != source_name:
            self.provider_impl.vfs.client.rename(self.file_id, name)
        self.provider_impl.vfs.invalidate(source_parent)
        self.provider_impl.vfs.invalidate(dest_parent)
        if not self.provider_impl.wait_for_entry(dest_parent, name):
            raise DAVError(HTTP_INTERNAL_ERROR)

    def get_used_bytes(self) -> int | None:
        return None

    def get_available_bytes(self) -> int | None:
        return None


class CloudFile(DAVNonCollection):
    def __init__(
        self,
        path: str,
        environ: dict,
        provider: "CloudProvider",
        entry: Entry,
    ):
        super().__init__(path, environ)
        self.provider_impl = provider
        self.entry = entry
        self._write_buffer: _PutBuffer | None = None
        self._write_lock = threading.Lock()
        self._write_target_parent: str = entry.parent_file_id or ROOT_FILE_ID
        self._write_name: str = entry.name

    def get_content_length(self) -> int:
        return self.entry.size

    def get_etag(self) -> str:
        return self.entry.etag or self.entry.file_id or ""

    def support_etag(self) -> bool:
        return bool(self.entry.file_id)

    def support_ranges(self) -> bool:
        return bool(self.entry.file_id)

    def support_recursive_move(self, dest_path: str) -> bool:
        return False

    def get_content(self):
        if not self.entry.file_id or self.entry.size == 0:
            return io.BytesIO(b"")
        return _RangeStream(
            self.provider_impl.vfs.client,
            self.entry.file_id,
            self.entry.size,
        )

    def begin_write(self, *, content_type: str | None = None):
        with self._write_lock:
            if self._write_buffer is not None:
                self._write_buffer.close()
            self._write_buffer = _PutBuffer(tempfile.SpooledTemporaryFile(max_size=UPLOAD_PART_SIZE, mode="w+b"))
            parent_file_id, name = self.provider_impl.vfs.parent_entry_at(self.path)
            self._write_target_parent = parent_file_id
            self._write_name = name or self.entry.name
            return self._write_buffer

    def end_write(self, *, with_errors: bool):
        with self._write_lock:
            buffer = self._write_buffer
            self._write_buffer = None
        if buffer is None:
            return
        try:
            if with_errors:
                return
            buffer.source.seek(0, 2)
            size = buffer.source.tell()
            buffer.source.seek(0)
            self._upload(buffer.source, size)
        finally:
            buffer.source.close()

    def _upload(self, source: tempfile.SpooledTemporaryFile, size: int):
        client = self.provider_impl.vfs.client
        digest = hashlib.sha256()
        while True:
            chunk = source.read(1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
        content_hash = digest.hexdigest()
        source.seek(0)
        part_count = max(1, (size + UPLOAD_PART_SIZE - 1) // UPLOAD_PART_SIZE)
        requested_parts = [
            {
                "partNumber": number,
                "partSize": min(UPLOAD_PART_SIZE, size - (number - 1) * UPLOAD_PART_SIZE),
                "parallelHashCtx": {"partOffset": (number - 1) * UPLOAD_PART_SIZE},
            }
            for number in range(1, part_count + 1)
        ]
        # 服务端 v1 拒绝 overwrite；覆盖语义用临时名上传，成功后再替换旧文件。
        upload_name = self._write_name
        old_file_id = self.entry.file_id
        if old_file_id:
            upload_name = f"__mcm_tmp_{uuid.uuid4().hex}_{self._write_name}"
        result = client.upload_create(
            self._write_target_parent,
            upload_name,
            size,
            content_hash=content_hash,
            content_hash_algorithm="SHA256",
            file_rename_mode="auto_rename",
            part_infos=requested_parts,
        )
        node = response_data(result)
        file_id = str(node.get("fileId") or node.get("id") or self.entry.file_id or "")
        if not file_id:
            raise DAVError(HTTP_INTERNAL_ERROR)
        upload_id = str(node.get("uploadId") or "")
        transfer_token = str(node.get("transferToken") or "")
        # file/create 命中秒传时没有 uploadId，参考项目直接完成 PUT。
        if not upload_id:
            self._finish_replace(file_id, old_file_id, self._write_name)
            self.provider_impl.vfs.invalidate(self._write_target_parent)
            return
        planned = {int(item.get("partNumber")): item for item in (node.get("partInfos") or []) if isinstance(item, dict)}
        for number, requested in enumerate(requested_parts, start=1):
            part_size = int(requested["partSize"])
            part_info = planned.get(number, {})
            url = upload_url(part_info)
            if not url:
                info = client.get_upload_url(file_id, [requested], upload_id=upload_id, transfer_token=transfer_token)
                node_info = response_data(info)
                transfer_token = str(node_info.get("transferToken") or transfer_token)
                part_info = pick_part_info(node_info, number)
                url = upload_url(part_info)
            if not url:
                raise DAVError(HTTP_INTERNAL_ERROR)
            resp = client.upload_part(url, _BoundedReader(source, part_size), length=part_size)
            raise_for_upload(resp)
        client.complete_upload(
            file_id,
            upload_id=upload_id,
            transfer_token=transfer_token,
            content_hash=content_hash,
        )
        self._finish_replace(file_id, old_file_id, self._write_name)
        self.provider_impl.vfs.invalidate(self._write_target_parent)

    def _finish_replace(self, new_file_id: str, old_file_id: str, name: str):
        client = self.provider_impl.vfs.client
        if old_file_id and new_file_id != old_file_id:
            client.trash([old_file_id])
            if not self.provider_impl.wait_for_absence(self._write_target_parent, name):
                raise DAVError(HTTP_INTERNAL_ERROR)
            client.rename(new_file_id, name)
            if not self.provider_impl.wait_for_entry(self._write_target_parent, name):
                raise DAVError(HTTP_INTERNAL_ERROR)

    def delete(self):
        self.provider_impl.vfs.client.trash([self.entry.file_id])
        parent_file_id, _ = self.provider_impl.vfs.parent_entry_at(self.path)
        self.provider_impl.vfs.invalidate(parent_file_id)

    def copy_move_single(self, dest_path: str, *, is_move: bool):
        dest_parent, name = self.provider_impl.vfs.parent_entry_at(dest_path)
        if not name:
            raise DAVError(HTTP_FORBIDDEN)

        url_info = self.provider_impl.vfs.client.get_download_url(self.entry.file_id)
        url = str((url_info.get("data") or {}).get("url") or "")
        if not url:
            raise DAVError(HTTP_NOT_FOUND)
        target_entry = self.provider_impl.ensure_file_target(dest_parent, name)
        target = CloudFile(dest_path, self.environ, self.provider_impl, target_entry)
        temp = tempfile.SpooledTemporaryFile(max_size=UPLOAD_PART_SIZE, mode="w+b")
        try:
            with self.provider_impl.vfs.client.download_stream(url) as resp:
                while True:
                    chunk = resp.raw.read(DOWNLOAD_CHUNK_SIZE)
                    if not chunk:
                        break
                    temp.write(chunk)
            temp.seek(0, 2)
            size = temp.tell()
            temp.seek(0)
            target._upload(temp, size)
        finally:
            temp.close()


class CloudProvider(DAVProvider):
    """通过 MCloudClient + Vfs 提供目录和文件。"""

    def __init__(self, client: MCloudClient, vfs: Vfs):
        super().__init__()
        self.client = client
        self.vfs = vfs
        self.readonly = False

    def wait_for_entry(self, parent_file_id: str, name: str, timeout: float = 15.0) -> Entry | None:
        deadline = time.monotonic() + timeout
        consecutive_hits = 0
        while True:
            self.vfs.invalidate(parent_file_id)
            entry = self.vfs.child(parent_file_id, name)
            if entry:
                consecutive_hits += 1
                if consecutive_hits >= 2:
                    return entry
            else:
                consecutive_hits = 0
            if time.monotonic() >= deadline:
                return None
            time.sleep(0.5)

    def wait_for_absence(self, parent_file_id: str, name: str, timeout: float = 15.0) -> bool:
        deadline = time.monotonic() + timeout
        consecutive_misses = 0
        while True:
            self.vfs.invalidate(parent_file_id)
            if self.vfs.child(parent_file_id, name) is None:
                consecutive_misses += 1
                if consecutive_misses >= 2:
                    return True
            else:
                consecutive_misses = 0
            if time.monotonic() >= deadline:
                return False
            time.sleep(0.5)

    def get_resource_inst(self, path: str, environ: dict):
        if path in ("", "/"):
            return CloudFolder("/", environ, self, ROOT_FILE_ID)
        parent_file_id, name = self.vfs.parent_entry_at(path)
        if not name:
            return None
        entry = self.vfs.child(parent_file_id, name)
        if entry is None:
            return None
        if entry.is_dir:
            return CloudFolder(path, environ, self, entry.file_id)
        return CloudFile(path, environ, self, entry)

    def ensure_file_target(self, parent_file_id: str, name: str) -> Entry:
        entry = self.vfs.child(parent_file_id, name)
        if entry and entry.is_dir:
            raise DAVError(HTTP_FORBIDDEN)
        if entry:
            return entry
        # PUT 先返回目标，等 end_write 再创建云端文件，避免 0 字节占位。
        return Entry(name=name, file_id="", parent_file_id=parent_file_id, is_dir=False, size=0)


def join_path(parent: str, name: str) -> str:
    if parent in ("", "/"):
        return f"/{name}"
    return f"{parent.rstrip('/')}/{name}"


def response_data(result: dict[str, Any]) -> dict[str, Any]:
    data = result.get("data")
    return data if isinstance(data, dict) else {}


def pick_part_info(node: dict[str, Any], part_number: int, fallback: dict[str, Any] | None = None):
    parts = node.get("partInfos") or []
    if isinstance(parts, list):
        for part in parts:
            if isinstance(part, dict) and int(part.get("partNumber") or part_number) == int(part_number):
                return part
        if parts and isinstance(parts[0], dict):
            return parts[0]
    return fallback or {}


def upload_url(part_info: dict[str, Any]) -> str:
    return str(part_info.get("uploadUrl") or part_info.get("cdnUploadUrl") or part_info.get("url") or "")


def etag_for_part(resp: Any, part_info: dict[str, Any]) -> str:
    headers = getattr(resp, "headers", None) or {}
    return str(headers.get("etag") or headers.get("ETag") or part_info.get("etag") or "")


def raise_for_upload(resp: Any) -> None:
    if int(getattr(resp, "status_code", 500)) >= 400:
        raise DAVError(HTTP_INTERNAL_ERROR)
        self._response = self.client.download_stream(self._url, headers=headers)
