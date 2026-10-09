"""WebDAV 服务器封装。"""

from __future__ import annotations

import threading
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
        self._thread: threading.Thread | None = None

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
        from cheroot import wsgi

        server = wsgi.Server((self.host, self.port), self.app or self._build_app())
        server.start()

    def start_background(self) -> None:
        if self._thread and self._thread.is_alive():
            return
        if self.app is None:
            self.app = self._build_app()
        self._thread = threading.Thread(target=self.serve_forever, daemon=True)
        self._thread.start()
