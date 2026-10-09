"""本地网页控制面板（仅监听 127.0.0.1）。

只使用标准库 http.server，不引入额外依赖。页面静态文件位于 web/index.html。
所有 POST 接口要求自定义请求头 X-MCM: 1，浏览器跨站请求无法携带该头，
从而防止其他网页在你不知情时触发挂载或登出。
"""

from __future__ import annotations

import json
import logging
import mimetypes
import os
import sys
import threading
import webbrowser
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from .api import MCloudClient
from .config import Config
from .errors import MCloudError
from .service import LOG_BUFFER, MountError, MountService, install_log_buffer

log = logging.getLogger("mcloudmount.panel")

DEFAULT_PANEL_PORT = 8390
CSRF_HEADER = "X-MCM"
MAX_BODY = 64 * 1024


def web_dir() -> Path:
    """开发环境读取仓库中的 web/；PyInstaller 打包后读取 _MEIPASS 中的副本。"""
    base = getattr(sys, "_MEIPASS", None)
    if base:
        return Path(base) / "web"
    return Path(__file__).resolve().parents[2] / "web"


class Panel:
    def __init__(self, service: MountService | None = None):
        install_log_buffer()
        self.service = service or MountService()
        self._busy = threading.Lock()
        self._pending_phone = ""

    # ------------------------------------------------------------ 业务
    def status(self) -> dict[str, Any]:
        self.service.check_health()
        data = self.service.snapshot()
        data["pending_phone"] = self._pending_phone
        return data

    def send_code(self, phone: str) -> dict[str, Any]:
        if not (len(phone) == 11 and phone.startswith("1") and phone.isdigit()):
            raise MCloudError("请输入 11 位中国大陆手机号")
        cfg = Config.load()
        MCloudClient(cfg).send_sms_code(phone)
        self._pending_phone = phone
        return {"sent": True}

    def verify_code(self, phone: str, code: str) -> dict[str, Any]:
        if not code.isdigit():
            raise MCloudError("验证码只能是数字")
        cfg = Config.load()
        client = MCloudClient(cfg)
        result = client.login_with_sms(phone, code)
        if not client.login_result_success(result):
            raise MCloudError("登录失败，请确认验证码是否正确")
        client.save_account(phone, result)
        client.resolve_connection()
        self._pending_phone = ""
        self.service.reload_config()
        return {"logged_in": True}

    def start_mount(self) -> dict[str, Any]:
        if not self._busy.acquire(blocking=False):
            raise MCloudError("正在处理上一个操作，请稍候")

        def _run() -> None:
            try:
                self.service.start()
            except MountError:
                pass  # 错误已写入 service.state
            except Exception as exc:  # noqa: BLE001
                log.exception("挂载异常")
                self.service._set("error", error=str(exc))  # noqa: SLF001
            finally:
                self._busy.release()

        threading.Thread(target=_run, name="mcm-mount", daemon=True).start()
        return {"started": True}

    def stop_mount(self) -> dict[str, Any]:
        if not self._busy.acquire(blocking=False):
            raise MCloudError("正在处理上一个操作，请稍候")
        try:
            self.service.stop()
        finally:
            self._busy.release()
        return {"stopped": True}

    def logout(self) -> dict[str, Any]:
        if self.service.state.phase in ("mounted", "starting"):
            self.stop_mount()
        cfg = Config.load()
        cfg.account = type(cfg.account)()
        cfg.save()
        self._pending_phone = ""
        self.service.reload_config()
        return {"logged_out": True}


def make_handler(panel: Panel, host_allow: set[str]):
    class Handler(BaseHTTPRequestHandler):
        server_version = "mCloudMountPanel/0.2"

        # ---- 工具 ----
        def log_message(self, fmt: str, *args) -> None:  # 静默默认访问日志
            log.debug("%s - %s", self.address_string(), fmt % args)

        def _send_json(self, code: int, payload: dict[str, Any]) -> None:
            body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            self.send_response(code)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)

        def _send_file(self, path: Path) -> None:
            if not path.is_file():
                self._send_json(404, {"error": "not found"})
                return
            data = path.read_bytes()
            ctype = mimetypes.guess_type(str(path))[0] or "application/octet-stream"
            self.send_response(200)
            self.send_header("Content-Type", f"{ctype}; charset=utf-8" if ctype.startswith("text/") else ctype)
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(data)

        def _host_ok(self) -> bool:
            host = (self.headers.get("Host") or "").split(":")[0]
            return host in host_allow

        def _read_json(self) -> dict[str, Any]:
            length = int(self.headers.get("Content-Length") or 0)
            if length > MAX_BODY:
                raise MCloudError("请求内容过大")
            raw = self.rfile.read(length) if length else b"{}"
            data = json.loads(raw or b"{}")
            return data if isinstance(data, dict) else {}

        # ---- 路由 ----
        def do_GET(self) -> None:  # noqa: N802
            if not self._host_ok():
                self._send_json(403, {"error": "forbidden host"})
                return
            path = urlparse(self.path).path
            try:
                if path == "/api/status":
                    self._send_json(200, panel.status())
                elif path == "/api/logs":
                    self._send_json(200, {"lines": LOG_BUFFER.tail(300)})
                elif path in ("/", "/index.html"):
                    self._send_file(web_dir() / "index.html")
                elif path.startswith("/static/"):
                    rel = path[len("/static/"):]
                    target = (web_dir() / rel).resolve()
                    if web_dir().resolve() not in target.parents:
                        self._send_json(403, {"error": "forbidden"})
                        return
                    self._send_file(target)
                else:
                    self._send_json(404, {"error": "not found"})
            except MCloudError as exc:
                self._send_json(400, {"error": exc.message})
            except Exception as exc:  # noqa: BLE001
                log.exception("GET %s 失败", path)
                self._send_json(500, {"error": str(exc)})

        def do_POST(self) -> None:  # noqa: N802
            if not self._host_ok():
                self._send_json(403, {"error": "forbidden host"})
                return
            if self.headers.get(CSRF_HEADER) != "1":
                self._send_json(403, {"error": "missing CSRF header"})
                return
            path = urlparse(self.path).path
            try:
                body = self._read_json()
                if path == "/api/login/send":
                    out = panel.send_code(str(body.get("phone") or "").strip())
                elif path == "/api/login/verify":
                    out = panel.verify_code(str(body.get("phone") or "").strip(), str(body.get("code") or "").strip())
                elif path == "/api/mount":
                    out = panel.start_mount()
                elif path == "/api/unmount":
                    out = panel.stop_mount()
                elif path == "/api/logout":
                    out = panel.logout()
                else:
                    self._send_json(404, {"error": "not found"})
                    return
                self._send_json(200, out)
            except MCloudError as exc:
                self._send_json(400, {"error": exc.message})
            except (ValueError, json.JSONDecodeError):
                self._send_json(400, {"error": "请求格式错误"})
            except Exception as exc:  # noqa: BLE001
                log.exception("POST %s 失败", path)
                self._send_json(500, {"error": str(exc)})

    return Handler


def serve(port: int = DEFAULT_PANEL_PORT, open_browser: bool = True) -> int:
    panel = Panel()
    allow = {"127.0.0.1", "localhost"}
    httpd = ThreadingHTTPServer(("127.0.0.1", port), make_handler(panel, allow))
    url = f"http://127.0.0.1:{port}/"
    print(f"控制面板已启动：{url}")
    print("关闭此窗口即可退出；退出前会自动卸载盘符。")
    if open_browser and os.environ.get("MCM_NO_BROWSER") != "1":
        threading.Timer(0.8, lambda: webbrowser.open(url)).start()
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        httpd.server_close()
        panel.service.stop()
    return 0
