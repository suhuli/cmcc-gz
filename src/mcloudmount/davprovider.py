"""把中国移动云盘映射为 WebDAV 目录。"""

from __future__ import annotations

import hashlib
import io
import logging
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
from .transport import default_http_headers
from .vfs import ROOT_FILE_ID, Entry, Vfs

log = logging.getLogger(__name__)

UPLOAD_PART_SIZE = 8 * 1024 * 1024
DOWNLOAD_CHUNK_SIZE = 1024 * 1024
WAIT_INTERVAL = 0.5


def _status_error(status: int) -> DAVError:
    """把云端 HTTP 状态映射为 WebDAV 状态，避免把 5xx 误报成 404。"""
    if status in (404, 410):
        return DAVError(HTTP_NOT_FOUND)
    if status in (401, 403):
        return DAVError(HTTP_FORBIDDEN)
    return DAVError(HTTP_INTERNAL_ERROR)


class _RangeStream:
    """通过 Range 请求读取云端文件；seek 时重新打开连接。"""

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
        response = self.client.download_stream(self._url, headers=headers)
        if response.status_code >= 400:
            response.close()
            raise _status_error(response.status_code)
        self._response = response
        # 服务端忽略 Range 时会返回 200 和完整内容：读掉前 position 字节再继续
        if self.position and response.status_code == 200:
            skip = self.position
            while skip > 0:
                chunk = response.raw.read(min(skip, DOWNLOAD_CHUNK_SIZE))
                if not chunk:
                    self._close_response()
                    raise DAVError(HTTP_INTERNAL_ERROR)
                skip -= len(chunk)
        elif self.position and response.status_code != 206:
            self._close_response()
            raise DAVError(HTTP_INTERNAL_ERROR)

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
    """对字节范围的只读视图，避免复制大分片。"""

    def __init__(self, source, length: int):
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
    """WsgiDAV 关闭返回的流后，PUT 的字节仍需可读。"""

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
        parent_file_id, name = self.provider_impl.vfs.parent_entry_at(self.path)
        self.provider_impl.vfs.client.trash([self.file_id])
        if parent_file_id and name:
            self.provider_impl.vfs.forget(parent_file_id, name)
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
    def __init__(self, path: str, environ: dict, provider: "CloudProvider", entry: Entry):
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
        # 返回 True 才会走 move_recursive（服务端移动，不下载再上传）
        return True

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

    def _upload(self, source, size: int):
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
        # 覆盖语义：先用临时名上传，成功后再与旧文件原子替换（见 _finish_replace）
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
        file_id = str(node.get("fileId") or node.get("id") or "")
        if not file_id:
            raise DAVError(HTTP_INTERNAL_ERROR)
        upload_id = str(node.get("uploadId") or "")
        transfer_token = str(node.get("transferToken") or "")
        # 秒传命中时没有 uploadId，文件已经存在，直接替换
        if upload_id:
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
        """原子替换：旧文件先改名为备份 -> 新文件改为目标名 -> 成功后删除备份。

        任何一步失败都会把旧文件改回原名，并清理刚上传的临时文件，不留残留。
        """
        if not old_file_id or new_file_id == old_file_id:
            return
        provider = self.provider_impl
        client = provider.vfs.client
        parent = self._write_target_parent
        backup = f"__mcm_bak_{uuid.uuid4().hex}_{name}"
        try:
            client.rename(old_file_id, backup)
        except Exception:
            self._discard(new_file_id)
            raise
        try:
            provider.vfs.invalidate(parent)
            if not provider.wait_for_absence(parent, name):
                raise DAVError(HTTP_INTERNAL_ERROR)
            client.rename(new_file_id, name)
            if not provider.wait_for_entry(parent, name):
                raise DAVError(HTTP_INTERNAL_ERROR)
        except Exception:
            try:
                client.rename(old_file_id, name)
                provider.vfs.invalidate(parent)
            except Exception as restore_exc:  # noqa: BLE001
                log.error("替换失败且无法还原旧文件，备份保留为 %s: %s", backup, restore_exc)
            self._discard(new_file_id)
            raise
        try:
            client.trash([old_file_id])
        except Exception as exc:  # noqa: BLE001
            log.warning("新文件已生效，但旧文件备份未能移入回收站（%s）: %s", backup, exc)
        provider.vfs.invalidate(parent)

    def _discard(self, file_id: str) -> None:
        try:
            self.provider_impl.vfs.client.trash([file_id])
        except Exception as exc:  # noqa: BLE001
            log.warning("无法清理失败上传的临时文件 %s: %s", file_id, exc)

    def delete(self):
        if not self.entry.file_id:
            raise DAVError(HTTP_NOT_FOUND)
        self.provider_impl.vfs.client.trash([self.entry.file_id])
        parent_file_id, name = self.provider_impl.vfs.parent_entry_at(self.path)
        if parent_file_id and name:
            self.provider_impl.vfs.forget(parent_file_id, name)
        self.provider_impl.vfs.invalidate(parent_file_id)

    def move_recursive(self, dest_path: str):
        provider = self.provider_impl
        dest_parent, name = provider.vfs.parent_entry_at(dest_path)
        if not name:
            raise DAVError(HTTP_FORBIDDEN)
        source_parent, source_name = provider.vfs.parent_entry_at(self.path)
        provider.vfs.client.move([self.entry.file_id], dest_parent)
        if not provider.wait_for_entry(dest_parent, source_name):
            raise DAVError(HTTP_INTERNAL_ERROR)
        if name != source_name:
            provider.vfs.client.rename(self.entry.file_id, name)
        provider.vfs.invalidate(source_parent)
        provider.vfs.invalidate(dest_parent)
        if not provider.wait_for_entry(dest_parent, name):
            raise DAVError(HTTP_INTERNAL_ERROR)

    def copy_move_single(self, dest_path: str, *, is_move: bool):
        """COPY 的单文件实现：下载到临时文件再上传（服务端无复制接口时的兜底）。"""
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
                if resp.status_code >= 400:
                    raise _status_error(resp.status_code)
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
            time.sleep(WAIT_INTERVAL)

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
            time.sleep(WAIT_INTERVAL)

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
        # PUT 先返回目标，等 end_write 再创建云端文件，避免 0 字节占位
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


def raise_for_upload(resp: Any) -> None:
    status = int(getattr(resp, "status_code", 500))
    if status >= 400:
        raise _status_error(status)
