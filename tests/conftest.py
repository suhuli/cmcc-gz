"""pytest 公共夹具：内存云盘 + 本地 WebDAV 服务。"""

from __future__ import annotations

import socket
import sys
from pathlib import Path

import pytest
import requests

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))
sys.path.insert(0, str(ROOT / "tests"))

from fake_cloud import FakeCloud  # noqa: E402
from mcloudmount.davserver import DavServer  # noqa: E402


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


@pytest.fixture
def cloud() -> FakeCloud:
    return FakeCloud()


@pytest.fixture
def dav(cloud: FakeCloud):
    port = free_port()
    server = DavServer(cloud, "127.0.0.1", port, "mcloud", "mcloud")
    assert server.start_background(wait=10), "WebDAV 服务未能启动"
    session = requests.Session()
    session.auth = ("mcloud", "mcloud")

    class Handle:
        base = f"http://127.0.0.1:{port}"
        cloud_ = cloud
        server_ = server
        http = session
        port_ = port

        def url(self, path: str) -> str:
            from urllib.parse import quote

            return self.base + quote(path, safe="/")

    try:
        yield Handle()
    finally:
        server.stop()
