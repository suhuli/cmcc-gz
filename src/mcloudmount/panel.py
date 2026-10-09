"""本地网页控制面板（仅监听 127.0.0.1）。

只使用标准库 http.server。页面位于 web/index.html。
所有 POST 接口要求自定义请求头 X-MCM: 1；浏览器的跨站请求无法携带该自定义头，
从而防止其他网页在你不知情时触发挂载、删除或登出。
"""

from __future__ import annotations

import json
import logging
import mimetypes
import os
import sys
import threading
import time
import webbrowser
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from logging.handlers import RotatingFileHandler
from pathlib import Path
from typing import Any, Callable
from urllib.parse import parse_qs, urlparse

from . import autostart, tray
from .api import MCloudClient
from .config import Config, config_dir
from .errors import MCloudError
from .service import LOG_BUFFER, MountError, MountService, install_log_buffer

log = logging.getLogger("mcloudmount.panel")

DEFAULT_PANEL_PORT = 8390
CSRF_HEADER = "X-MCM"
MAX_BODY = 64 * 1024
INVALID_NAME_CHARS = set('\\/:*?"<>|')
ROOT_ID = "/"


def web_dir() -> Path:
    """开发环境读取仓库中的 web/；PyInstaller 打包后读取 _MEIPASS 中的副本。"""
    base = getattr(sys, "_MEIPASS", None)
    if base:
        return Path(base) / "web"
    return Path(__file__).resolve().parents[2] / "web"


def validate_name(name: str) -> str:
    name = (name or "").strip()
    if not name or len(name) > 255 or name in (".", "..") or any(c in INVALID_NAME_CHARS for c in name):
        raise MCloudError("名称不能为空，且不能包含 \\ / : * ? \" < > |")
    return name


def _is_dir(item: dict[str, Any]) -> bool:
    kind = str(item.get("type") or "").lower()
    return kind in ("folder", "dir") or item.get("systemDir") is True


class Panel:
    def __init__(
        self,
        service: MountService | None = None,
        client_factory: Callable[[], MCloudClient] | None = None,
    ):
        install_log_buffer()
        self.service = service or MountService()
        self._client_factory = client_factory or (lambda: MCloudClient(Config.load()))
        self._busy = threading.Lock()
        self._pending_phone = ""
        self._connected = False

    # ------------------------------------------------------------ 账号
    def status(self) -> dict[str, Any]:
        self.service.check_health()
        data = self.service.snapshot()
        data["pending_phone"] = self._pending_phone
        return data

    def send_code(self, phone: str) -> dict[str, Any]:
        if not (len(phone) == 11 and phone.startswith("1") and phone.isdigit()):
            raise MCloudError("请输入 11 位中国大陆手机号")
        MCloudClient(Config.load()).send_sms_code(phone)
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
        self._connected = False
        self.service.reload_config()
        return {"logged_in": True}

    def logout(self) -> dict[str, Any]:
        if self.service.state.phase in ("mounted", "starting"):
            self.stop_mount()
        cfg = Config.load()
        cfg.account = type(cfg.account)()
        cfg.save()
        self._pending_phone = ""
        self._connected = False
        self.service.reload_config()
        return {"logged_out": True}

    # ------------------------------------------------------------ 挂载
    def start_mount(self) -> dict[str, Any]:
        if not self._busy.acquire(blocking=False):
            raise MCloudError("正在处理上一个操作，请稍候")

        def _run() -> None:
            try:
                self.service.start()
                self._connected = True
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
        """卸载在后台线程执行：请求立即返回，界面通过 status 轮询进度。"""
        if not self._busy.acquire(blocking=False):
            raise MCloudError("正在处理上一个操作，请稍候")

        def _run() -> None:
            try:
                self.service.stop()
            except Exception as exc:  # noqa: BLE001
                log.exception("卸载失败")
                self.service._set("mounted" if self.service.drive_mounted() else "error",  # noqa: SLF001
                                  error=f"卸载失败：{exc}")
            finally:
                self._busy.release()

        threading.Thread(target=_run, name="mcm-unmount", daemon=True).start()
        return {"stopping": True}

    def toggle_mount(self) -> None:
        if self.service.state.phase == "mounted":
            self.stop_mount()
        else:
            self.start_mount()

    def open_drive(self) -> dict[str, Any]:
        drive = self.service.cfg.mount.drive
        if not drive or not self.service.drive_mounted():
            raise MCloudError("盘符尚未挂载")
        if os.name == "nt":
            os.startfile(drive + "\\")  # noqa: S606 - 打开本地盘符
        return {"opened": drive}

    # ------------------------------------------------------------ 文件
    def _client(self) -> MCloudClient:
        cfg = Config.load()
        if not (cfg.account.phone and cfg.account.token):
            raise MCloudError("请先登录")
        if cfg.account.token_expire_ms and cfg.account.token_expire_ms <= time.time() * 1000:
            raise MCloudError("登录已过期，请重新登录")
        client = self._client_factory()
        if not self._connected:
            client.resolve_connection()
            self._connected = True
        return client

    def list_files(self, folder: str) -> dict[str, Any]:
        folder = folder or ROOT_ID
        client = self._client()
        items = []
        for raw in client.iter_folder_items(folder):
            file_id = str(raw.get("fileId") or "")
            name = str(raw.get("name") or "")
            if not file_id or not name:
                continue
            items.append({
                "id": file_id,
                "name": name,
                "is_dir": _is_dir(raw),
                "size": int(raw.get("size") or 0),
                "updated": raw.get("updatedAt") or raw.get("localUpdatedAt") or "",
            })
        items.sort(key=lambda x: (not x["is_dir"], x["name"].lower()))
        return {"folder": folder, "items": items}

    def mkdir(self, parent: str, name: str) -> dict[str, Any]:
        name = validate_name(name)
        self._client().create_folder(parent or ROOT_ID, name)
        return {"created": name}

    def rename(self, file_id: str, name: str) -> dict[str, Any]:
        if not file_id:
            raise MCloudError("缺少文件 ID")
        name = validate_name(name)
        self._client().rename(file_id, name)
        return {"renamed": name}

    def delete(self, file_ids: list[str]) -> dict[str, Any]:
        ids = [str(x) for x in file_ids if x]
        if not ids:
            raise MCloudError("没有选中的文件")
        self._client().trash(ids)
        return {"deleted": len(ids)}

    # ------------------------------------------------------------ 设置
    def settings(self) -> dict[str, Any]:
        cfg = Config.load()
        return {
            "autostart": autostart.is_enabled(),
            "autostart_supported": os.name == "nt",
            "auto_mount": bool(cfg.auto_mount),
            "tray_available": tray.is_available(),
            "drive": cfg.mount.drive,
        }

    def update_settings(self, body: dict[str, Any]) -> dict[str, Any]:
        cfg = Config.load()
        if "auto_mount" in body:
            cfg.auto_mount = bool(body["auto_mount"])
            cfg.save()
            self.service.reload_config()
        if "autostart" in body:
            if os.name != "nt":
                raise MCloudError("开机自启仅支持 Windows")
            autostart.set_enabled(bool(body["autostart"]))
        return self.settings()


def make_handler(panel: Panel, host_allow: set[str]):
    class Handler(BaseHTTPRequestHandler):
        server_version = "mCloudMountPanel/0.3"

        def log_message(self, fmt: str, *args) -> None:
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
            if ctype.startswith("text/") or ctype in ("application/javascript", "application/json"):
                ctype = f"{ctype}; charset=utf-8"
            self.send_response(200)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(data)

        def _host_ok(self) -> bool:
            host = (self.headers.get("Host") or "").rsplit(":", 1)[0]
            return host in host_allow

        def _read_json(self) -> dict[str, Any]:
            length = int(self.headers.get("Content-Length") or 0)
            if length > MAX_BODY:
                raise MCloudError("请求内容过大")
            raw = self.rfile.read(length) if length else b"{}"
            data = json.loads(raw or b"{}")
            return data if isinstance(data, dict) else {}

        def do_GET(self) -> None:  # noqa: N802
            if not self._host_ok():
                self._send_json(403, {"error": "forbidden host"})
                return
            parsed = urlparse(self.path)
            path = parsed.path
            query = parse_qs(parsed.query)
            try:
                if path == "/api/status":
                    self._send_json(200, panel.status())
                elif path == "/api/logs":
                    self._send_json(200, {"lines": LOG_BUFFER.tail(300)})
                elif path == "/api/files":
                    self._send_json(200, panel.list_files((query.get("folder") or [ROOT_ID])[0]))
                elif path == "/api/settings":
                    self._send_json(200, panel.settings())
                elif path in ("/", "/index.html"):
                    self._send_file(web_dir() / "index.html")
                elif path.startswith("/static/"):
                    base = web_dir().resolve()
                    target = (base / path[len("/static/"):]).resolve()
                    if base not in target.parents:
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
                elif path == "/api/logout":
                    out = panel.logout()
                elif path == "/api/mount":
                    out = panel.start_mount()
                elif path == "/api/unmount":
                    out = panel.stop_mount()
                elif path == "/api/open-drive":
                    out = panel.open_drive()
                elif path == "/api/files/mkdir":
                    out = panel.mkdir(str(body.get("parent") or ROOT_ID), str(body.get("name") or ""))
                elif path == "/api/files/rename":
                    out = panel.rename(str(body.get("id") or ""), str(body.get("name") or ""))
                elif path == "/api/files/delete":
                    ids = body.get("ids") or []
                    out = panel.delete(ids if isinstance(ids, list) else [])
                elif path == "/api/settings":
                    out = panel.update_settings(body)
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


def _attach_file_log() -> None:
    try:
        log_dir = config_dir()
        log_dir.mkdir(parents=True, exist_ok=True)
        handler = RotatingFileHandler(log_dir / "mcloudmount.log", maxBytes=2 * 1024 * 1024, backupCount=2, encoding="utf-8")
        handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(name)s %(message)s"))
        logging.getLogger().addHandler(handler)
    except OSError:
        pass


def serve(
    port: int = DEFAULT_PANEL_PORT,
    *,
    open_browser: bool = True,
    use_tray: bool = True,
    background: bool = False,
) -> int:
    install_log_buffer()
    _attach_file_log()
    panel = Panel()
    allow = {"127.0.0.1", "localhost"}
    httpd = ThreadingHTTPServer(("127.0.0.1", port), make_handler(panel, allow))
    url = f"http://127.0.0.1:{port}/"
    log.info("控制面板已启动：%s", url)

    if panel.service.cfg.auto_mount and panel.service.logged_in():
        try:
            panel.start_mount()
        except MCloudError as exc:
            log.warning("自动挂载未执行: %s", exc.message)

    server_thread = threading.Thread(target=httpd.serve_forever, name="mcm-panel", daemon=True)
    server_thread.start()

    if open_browser and not background and os.environ.get("MCM_NO_BROWSER") != "1":
        threading.Timer(0.8, lambda: webbrowser.open(url)).start()

    done = threading.Event()

    def _shutdown() -> None:
        if done.is_set():
            return
        done.set()
        httpd.shutdown()
        httpd.server_close()
        panel.service.stop()

    try:
        if use_tray and tray.is_available():
            def _label() -> str:
                return "卸载盘符" if panel.service.state.phase == "mounted" else "挂载盘符"

            def _open_drive() -> None:
                try:
                    panel.open_drive()
                except MCloudError as exc:
                    log.warning(exc.message)

            tray.run_tray(
                url,
                on_toggle_mount=panel.toggle_mount,
                mounted_label=_label,
                on_quit=_shutdown,
                on_open_drive=_open_drive,
            )
        else:
            print(f"控制面板已启动：{url}")
            print("按 Ctrl+C 退出；退出前会自动卸载盘符。")
            while server_thread.is_alive():
                time.sleep(1)
    except KeyboardInterrupt:
        pass
    finally:
        _shutdown()
    return 0
