"""Offline smoke test for the WsgiDAV adapter using a fake cloud client."""

from __future__ import annotations

import io
import sys
import time
from pathlib import Path
from typing import Any

import requests

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from mcloudmount.config import Account, Config
from mcloudmount.davserver import DavServer


class FakeResponse:
    def __init__(self, data: bytes = b"", status: int = 200):
        self.status_code = status
        self.headers = {"ETag": "fake"}
        self.raw = io.BytesIO(data)

    def iter_content(self, size: int):
        while True:
            chunk = self.raw.read(size)
            if not chunk:
                break
            yield chunk

    def close(self):
        self.raw.close()


class FakeClient:
    def __init__(self):
        self.config = Config(account=Account(phone="13000000000", token="fake"))
        self.children: dict[str, list[dict[str, Any]]] = {"/": []}
        self.next_id = 1
        self.parts: dict[str, dict[int, bytes]] = {}
        self.completed: list[tuple[str, str, str, str]] = []

    def _node(self, parent: str, name: str, is_dir: bool, size: int = 0):
        file_id = str(self.next_id)
        self.next_id += 1
        node = {
            "name": name,
            "fileId": file_id,
            "parentFileId": parent,
            "type": "folder" if is_dir else "file",
            "size": size,
        }
        self.children.setdefault(parent, []).append(node)
        if is_dir:
            self.children[file_id] = []
        return node

    def iter_folder_items(self, parent_file_id: str, **_: Any):
        return list(self.children.get(parent_file_id, []))

    def create_folder(self, parent_file_id: str, name: str, **_: Any):
        node = self._node(parent_file_id, name, True)
        return {"data": node}

    def upload_create(self, parent_file_id: str, name: str, size: int, **_: Any):
        node = self._node(parent_file_id, name, False, size)
        node["uploadId"] = f"upload-{node['fileId']}"
        node["transferToken"] = f"token-{node['fileId']}"
        node["partInfos"] = [{"partNumber": 1, "partSize": size, "uploadUrl": f"fake://{node['fileId']}/1"}]
        return {"data": node}

    def upload_part(self, upload_url: str, data: Any, length: int | None = None, **_: Any):
        file_id, part_number = upload_url.removeprefix("fake://").split("/")
        self.parts.setdefault(file_id, {})[int(part_number)] = data.read(length or -1)
        return FakeResponse(b"uploaded")

    def complete_upload(self, file_id: str, upload_id: str = "", transfer_token: str = "", content_hash: str = ""):
        total = sum(len(v) for v in self.parts.get(file_id, {}).values())
        self.completed.append((file_id, upload_id, transfer_token, content_hash))
        for parent, nodes in self.children.items():
            for node in nodes:
                if node.get("fileId") == file_id:
                    node["size"] = total
                    break
        return {"data": {"fileId": file_id}}

    def get_download_url(self, file_id: str, expire_sec: int = 3600):
        return {"data": {"url": f"fake-download://{file_id}"}}

    def download_stream(self, url: str, headers=None, **_: Any):
        file_id = url.removeprefix("fake-download://")
        data = b"".join(self.parts.get(file_id, {}).values())
        return FakeResponse(data)

    def trash(self, file_ids: list[str]):
        for parent, nodes in self.children.items():
            self.children[parent] = [node for node in nodes if node.get("fileId") not in file_ids]
        return {"data": {}}


def wait_server(port: int, timeout: float = 5.0) -> bool:
    deadline = time.time() + timeout
    session = requests.Session()
    session.auth = ("mcloud", "mcloud")
    while time.time() < deadline:
        try:
            response = session.request("PROPFIND", f"http://127.0.0.1:{port}/", timeout=1)
            if response.status_code < 500:
                return True
        except requests.RequestException:
            time.sleep(0.1)
    return False


def main() -> int:
    client = FakeClient()
    port = 18380
    server = DavServer(client, "127.0.0.1", port, "mcloud", "mcloud")
    server.start_background()
    if not wait_server(port):
        print("server failed to start")
        return 1

    session = requests.Session()
    session.auth = ("mcloud", "mcloud")
    base = f"http://127.0.0.1:{port}"

    checks: list[tuple[str, bool, str]] = []
    response = session.request("MKCOL", f"{base}/offline")
    checks.append(("mkdir", response.status_code == 201, response.text[:100]))
    checks.append(("dir cached", any(n.get("name") == "offline" for n in client.children["/"]), ""))

    payload = b"mcloud offline payload"
    response = session.put(f"{base}/offline/test.bin", data=payload)
    checks.append(("put", response.status_code in (200, 201, 204), response.text[:100]))
    checks.append(("upload completed", bool(client.completed and client.completed[0][0] == "2"), ""))
    checks.append(("upload bytes", sum(map(len, client.parts.get("2", {}).values())) == len(payload), ""))

    response = session.get(f"{base}/offline/test.bin")
    checks.append(("get", response.content == payload, response.text[:100]))

    response = session.request("PROPFIND", f"{base}/offline", headers={"Depth": "1"})
    checks.append(("propfind", response.status_code == 207, response.text[:100]))

    response = session.delete(f"{base}/offline/test.bin")
    checks.append(("delete", response.status_code in (200, 204), response.text[:100]))
    response = session.delete(f"{base}/offline")
    checks.append(("delete dir", response.status_code in (200, 204), response.text[:100]))

    failed = [name for name, ok, _ in checks if not ok]
    for name, ok, detail in checks:
        print(f"{'PASS' if ok else 'FAIL'} {name} {detail}")
    print(f"passed={len(checks) - len(failed)}/{len(checks)}")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
