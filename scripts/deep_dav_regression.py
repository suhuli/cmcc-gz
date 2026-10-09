"""Deep regression test against the real MCloud API and local WsgiDAV server."""

from __future__ import annotations

import os
import random
import sys
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from urllib.parse import quote

import requests

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from mcloudmount.api import MCloudClient
from mcloudmount.config import Config
from mcloudmount.davserver import DavServer
from mcloudmount.vfs import Vfs


def wait_server(port: int, timeout: float = 10.0) -> bool:
    deadline = time.time() + timeout
    session = requests.Session()
    session.auth = ("mcloud", "mcloud")
    while time.time() < deadline:
        try:
            response = session.request("PROPFIND", f"http://127.0.0.1:{port}/", headers={"Depth": "0"}, timeout=3)
            if response.status_code < 500:
                return True
        except requests.RequestException:
            time.sleep(0.1)
    return False


def url_path(path: str) -> str:
    return quote(path, safe="/")


class DeepRegression:
    def __init__(self):
        self.cfg = Config.load()
        self.client = MCloudClient(self.cfg)
        self.vfs = Vfs(self.client)
        self.port = int(os.environ.get("MCLOUD_TEST_PORT", "18381"))
        self.base = f"http://127.0.0.1:{self.port}"
        self.session = requests.Session()
        self.session.auth = ("mcloud", "mcloud")
        self.root_name = f"__mcm_regression_{uuid.uuid4().hex[:8]}"
        self.results: list[tuple[str, bool, str]] = []

    def check(self, name: str, ok: bool, detail: str = "") -> None:
        self.results.append((name, bool(ok), detail[:200]))
        print(f"{'PASS' if ok else 'FAIL'} {name}{'' if ok else ' ' + detail[:200]}")

    def request(self, method: str, path: str, **kwargs):
        return self.session.request(
            method,
            f"{self.base}{url_path(path)}",
            timeout=kwargs.pop("timeout", 90),
            **kwargs,
        )

    def put(self, path: str, data: bytes) -> None:
        response = self.request("PUT", path, data=data, timeout=180)
        if response.status_code not in (200, 201, 204):
            raise AssertionError(f"PUT {path}: {response.status_code} {response.text[:200]}")

    def get(self, path: str) -> bytes:
        response = self.request("GET", path)
        if response.status_code != 200:
            raise AssertionError(f"GET {path}: {response.status_code} {response.text[:200]}")
        return response.content

    def mkcol(self, path: str) -> None:
        response = self.request("MKCOL", path)
        if response.status_code != 201:
            raise AssertionError(f"MKCOL {path}: {response.status_code} {response.text[:200]}")

    def move(self, source: str, target: str, overwrite: bool = False) -> None:
        response = self.session.request(
            "MOVE",
            f"{self.base}{url_path(source)}",
            headers={
                "Destination": f"{self.base}{url_path(target)}",
                "Overwrite": "T" if overwrite else "F",
            },
            timeout=90,
        )
        if response.status_code not in (200, 201, 204):
            raise AssertionError(f"MOVE {source} -> {target}: {response.status_code} {response.text[:200]}")

    def copy(self, source: str, target: str) -> None:
        response = self.session.request(
            "COPY",
            f"{self.base}{url_path(source)}",
            headers={"Destination": f"{self.base}{url_path(target)}", "Overwrite": "F"},
            timeout=180,
        )
        if response.status_code not in (200, 201, 204):
            raise AssertionError(f"COPY {source} -> {target}: {response.status_code} {response.text[:200]}")

    def delete(self, path: str) -> None:
        response = self.request("DELETE", path, timeout=120)
        if response.status_code not in (200, 204):
            raise AssertionError(f"DELETE {path}: {response.status_code} {response.text[:200]}")

    def run(self) -> int:
        if not self.cfg.account.phone or not self.cfg.account.token:
            print("FAIL login state missing")
            return 2

        self.client.resolve_connection()
        server = DavServer(self.client, "127.0.0.1", self.port, "mcloud", "mcloud")
        server.start_background()
        if not wait_server(self.port):
            print("FAIL local DAV server did not start")
            return 2

        try:
            self.mkcol(f"/{self.root_name}")
            self.check("root mkdir", self.vfs.entry_at(f"/{self.root_name}") is not None)

            self.mkcol(f"/{self.root_name}/folderA")
            self.mkcol(f"/{self.root_name}/folderB")
            self.check("nested mkdir", True)

            alpha = random.randbytes(64 * 1024)
            self.put(f"/{self.root_name}/folderA/alpha.bin", alpha)
            self.check("put/read", self.get(f"/{self.root_name}/folderA/alpha.bin") == alpha)

            self.move(f"/{self.root_name}/folderA/alpha.bin", f"/{self.root_name}/folderB/renamed.bin")
            self.vfs.invalidate("/")
            folder_a_id = self.vfs.entry_at(f"/{self.root_name}/folderA").file_id
            folder_b_id = self.vfs.entry_at(f"/{self.root_name}/folderB").file_id
            a_items = {item.get("name") for item in self.client.iter_folder_items(folder_a_id)}
            b_items = {item.get("name") for item in self.client.iter_folder_items(folder_b_id)}
            root_items = {item.get("name") for item in self.client.iter_folder_items("/")}
            self.check("move cloud state", "renamed.bin" in b_items and "alpha.bin" not in a_items, f"A={a_items} B={b_items} root={root_items}")
            self.check("file move+rename", self.get(f"/{self.root_name}/folderB/renamed.bin") == alpha)
            self.vfs.invalidate("/")
            items = {item.get("name") for item in self.client.iter_folder_items(self.vfs.entry_at(f"/{self.root_name}/folderB").file_id)}
            self.check("rename exact name", "renamed.bin" in items and "alpha.bin" not in items, str(items))

            self.copy(f"/{self.root_name}/folderB/renamed.bin", f"/{self.root_name}/folderA/copied.bin")
            self.check("file copy", self.get(f"/{self.root_name}/folderA/copied.bin") == alpha)

            first = random.randbytes(128 * 1024)
            second = random.randbytes(192 * 1024)
            self.put(f"/{self.root_name}/folderA/overwrite.bin", first)
            self.put(f"/{self.root_name}/folderA/overwrite.bin", second)
            self.check("overwrite", self.get(f"/{self.root_name}/folderA/overwrite.bin") == second)

            self.put(f"/{self.root_name}/folderA/empty.bin", b"")
            self.check("zero byte", self.get(f"/{self.root_name}/folderA/empty.bin") == b"")

            chinese = random.randbytes(17 * 1024)
            self.put(f"/{self.root_name}/folderA/测试文件.bin", chinese)
            self.check("chinese filename", self.get(f"/{self.root_name}/folderA/测试文件.bin") == chinese)

            large = random.randbytes(9 * 1024 * 1024 + 1234)
            self.put(f"/{self.root_name}/folderA/large.bin", large)
            self.check("multipart upload", self.get(f"/{self.root_name}/folderA/large.bin") == large)

            range_response = self.session.get(
                f"{self.base}{url_path(f'/{self.root_name}/folderA/large.bin')}",
                headers={"Range": "bytes=1048576-1048595"},
                timeout=90,
            )
            self.check(
                "range read",
                range_response.status_code == 206
                and range_response.content == large[1048576:1048596]
                and range_response.headers.get("Content-Range", "").startswith("bytes 1048576-"),
                f"{range_response.status_code} {range_response.headers.get('Content-Range', '')}",
            )

            concurrent = {f"parallel_{index}.bin": random.randbytes(1024 * 1024) for index in range(4)}
            with ThreadPoolExecutor(max_workers=4) as pool:
                futures = [pool.submit(self.put, f"/{self.root_name}/folderA/{name}", data) for name, data in concurrent.items()]
                for future in futures:
                    future.result()
            with ThreadPoolExecutor(max_workers=4) as pool:
                futures = [pool.submit(self.get, f"/{self.root_name}/folderA/{name}") for name in concurrent]
                actual = [future.result() for future in futures]
            self.check(
                "concurrent read/write",
                actual == [concurrent[name] for name in concurrent],
            )

            self.mkcol(f"/{self.root_name}/folderA/deep")
            deep = random.randbytes(4096)
            self.put(f"/{self.root_name}/folderA/deep/deep.bin", deep)
            self.check("deep path", self.get(f"/{self.root_name}/folderA/deep/deep.bin") == deep)

            self.mkcol(f"/{self.root_name}/dirmove")
            self.put(f"/{self.root_name}/dirmove/inside.bin", alpha)
            self.move(f"/{self.root_name}/dirmove", f"/{self.root_name}/folderB/moveddir")
            self.check("directory move", self.get(f"/{self.root_name}/folderB/moveddir/inside.bin") == alpha)

            self.copy(f"/{self.root_name}/folderB/moveddir", f"/{self.root_name}/folderA/copieddir")
            self.check("directory copy", self.get(f"/{self.root_name}/folderA/copieddir/inside.bin") == alpha)

            propfind = self.request("PROPFIND", f"/{self.root_name}/folderA", headers={"Depth": "1"})
            self.check("propfind", propfind.status_code == 207 and "large.bin" in propfind.text, f"{propfind.status_code}")

            listing = [item.get("name") for item in self.client.iter_folder_items(self.vfs.entry_at(f"/{self.root_name}/folderA").file_id)]
            self.check(
                "no temporary residue",
                not any(str(name).startswith("__mcm_tmp_") for name in listing),
                str(listing),
            )
        except Exception as exc:
            self.check("uncaught regression", False, repr(exc))
        finally:
            self.cleanup()

        failed = [name for name, ok, _ in self.results if not ok]
        print(f"passed={len(self.results) - len(failed)}/{len(self.results)}")
        return 1 if failed else 0

    def cleanup(self) -> None:
        try:
            self.vfs.invalidate("/")
            entries = list(self.client.iter_folder_items("/"))
            roots = [entry for entry in entries if str(entry.get("name") or "").startswith("__mcm_regression_")]
            if roots:
                self.client.trash([str(entry.get("fileId")) for entry in roots if entry.get("fileId")])
                self.vfs.invalidate("/")
            self.check("cleanup", True)
        except Exception as exc:
            self.check("cleanup", False, repr(exc))


if __name__ == "__main__":
    raise SystemExit(DeepRegression().run())
