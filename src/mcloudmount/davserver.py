"""WebDAV 服务器封装：可启动、等待就绪、可停止。"""

from __future__ import annotations

import threading
import time
from typing import Any

from wsgidav.wsgidav_app import WsgiDAVApp

from .api import MCloudClient
from .davprovider import CloudProvider
from .vfs import Vfs


class DavServer:
    """在本地端口暴露云盘内容。"""

    def __init__(self, client: MCloudClient, host: str, port: int, user: str, password: str):
        self.client = client
        self.host = host
        self.port = port
        self.user = user
        self.password = password
        self.provider = CloudProvider(client, Vfs(client))
        self.app: WsgiDAVApp | None = None
        self._server: Any = None
        self._thread: threading.Thread | None = None
        self._lock = threading.Lock()

    def _build_app(self) -> WsgiDAVApp:
        config: dict[str, Any] = {
            "host": self.host,
            "port": self.port,
            "provider_mapping": {"/": self.provider},
            "http_authenticator": {
                "domain": "mCloudMount",
                "accept_basic": True,
                "accept_digest": True,
                "default_to_digest": True,
            },
            "simple_dc": {
                "user_mapping": {
                    "*": {
                        self.user: {"password": self.password},
                    }
                }
            },
            "verbose": 1,
            "logging": {
                "enable_loggers": [],
            },
            "property_manager": True,
            "lock_storage": True,
        }
        return WsgiDAVApp(config)

    def start(self) -> None:
        if self.app is None:
            self.app = self._build_app()

    def serve_forever(self) -> None:
        """阻塞运行，直到 stop() 被调用。"""
        from cheroot import wsgi

        with self._lock:
            if self.app is None:
                self.app = self._build_app()
            self._server = wsgi.Server((self.host, self.port), self.app)
            server = self._server
        server.start()

    def start_background(self, wait: float = 5.0) -> bool:
        """后台启动并等待端口就绪。返回是否就绪。"""
        if self._thread and self._thread.is_alive():
            return self.is_ready()
        self.start()
        self._thread = threading.Thread(target=self.serve_forever, name="mcm-dav", daemon=True)
        self._thread.start()
        deadline = time.monotonic() + wait
        while time.monotonic() < deadline:
            if self.is_ready():
                return True
            if not self._thread.is_alive():
                return False
            time.sleep(0.05)
        return False

    def is_ready(self) -> bool:
        server = self._server
        return bool(server is not None and getattr(server, "ready", False))

    def is_running(self) -> bool:
        return bool(self._thread and self._thread.is_alive())

    def stop(self, timeout: float = 5.0) -> None:
        server = self._server
        if server is not None:
            server.stop()
        if self._thread is not None:
            self._thread.join(timeout=timeout)
        self._server = None
        self._thread = None
