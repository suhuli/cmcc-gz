"""挂载服务：把“验证登录 -> 启动 WebDAV -> 挂载盘符”的流程封装成可复用对象。

CLI 与网页面板共用此模块，保证行为一致。
"""

from __future__ import annotations

import logging
import threading
import time
from collections import deque
from dataclasses import dataclass, field

from .api import MCloudClient
from .config import Config
from .davserver import DavServer
from .errors import MCloudError
from .winutil import ensure_webclient, mount, mount_status, set_webclient_limits, umount, wait_mount

log = logging.getLogger("mcloudmount")


class MountError(MCloudError):
    """挂载流程中的可预期失败，message 可直接展示给用户。"""


class LogBuffer(logging.Handler):
    """把最近的日志保存在内存里，供面板显示。"""

    def __init__(self, maxlen: int = 300):
        super().__init__()
        self.records: deque[str] = deque(maxlen=maxlen)
        self.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(message)s", "%H:%M:%S"))

    def emit(self, record: logging.LogRecord) -> None:
        try:
            self.records.append(self.format(record))
        except Exception:  # noqa: BLE001
            pass

    def tail(self, n: int = 200) -> list[str]:
        return list(self.records)[-n:]


LOG_BUFFER = LogBuffer()


def install_log_buffer() -> None:
    root = logging.getLogger()
    if LOG_BUFFER not in root.handlers:
        root.addHandler(LOG_BUFFER)
        root.setLevel(logging.INFO)


@dataclass
class MountState:
    phase: str = "idle"  # idle | starting | mounted | stopping | error
    message: str = ""
    error: str = ""
    since: float = field(default_factory=time.time)


class MountService:
    def __init__(self, cfg: Config | None = None):
        self.cfg = cfg or Config.load()
        self._lock = threading.Lock()
        self._server: DavServer | None = None
        self._client: MCloudClient | None = None
        self.state = MountState()

    # ------------------------------------------------------------ 状态
    def token_expired(self) -> bool:
        expire = self.cfg.account.token_expire_ms
        return bool(expire and expire <= time.time() * 1000)

    def logged_in(self) -> bool:
        return bool(self.cfg.account.phone and self.cfg.account.token)

    def drive_mounted(self) -> bool:
        return bool(self.cfg.mount.drive and mount_status(self.cfg.mount.drive))

    def server_running(self) -> bool:
        return bool(self._server and self._server.is_running())

    def snapshot(self) -> dict:
        return {
            "logged_in": self.logged_in(),
            "account": self.cfg.account.phone,
            "token_expired": self.token_expired(),
            "drive": self.cfg.mount.drive,
            "webdav": f"http://{self.cfg.mount.host}:{self.cfg.mount.port}",
            "phase": self.state.phase,
            "message": self.state.message,
            "error": self.state.error,
            "server_running": self.server_running(),
            "mounted": self.state.phase == "mounted" and self.drive_mounted(),
        }

    def _set(self, phase: str, message: str = "", error: str = "") -> None:
        self.state = MountState(phase=phase, message=message, error=error)
        log.info("%s %s", phase, message or error)

    # ------------------------------------------------------------ 操作
    def start(self) -> None:
        """同步执行挂载流程；失败抛 MountError。成功后 WebDAV 服务与盘符保持运行。"""
        with self._lock:
            if not self.logged_in():
                self._set("error", error="请先登录")
                raise MountError("请先登录")
            if self.token_expired():
                self._set("error", error="登录已过期，请重新登录")
                raise MountError("登录已过期，请重新登录")
            self._set("starting", "正在准备 WebClient 服务")
            try:
                if not ensure_webclient():
                    log.warning("WebClient 服务未能启动，挂载可能失败")
                set_webclient_limits()
                client = MCloudClient(self.cfg)
                self._set("starting", "正在验证登录状态")
                client.resolve_connection()
                drive = self.cfg.mount.drive
                if drive and mount_status(drive):
                    umount(drive)
                server = DavServer(client, self.cfg.mount.host, self.cfg.mount.port,
                                   self.cfg.mount.dav_user, self.cfg.mount.dav_password)
                self._set("starting", "正在启动本地 WebDAV 服务")
                if not server.start_background():
                    raise MountError(f"WebDAV 服务启动失败，端口 {self.cfg.mount.port} 可能被占用")
                if drive:
                    self._set("starting", f"正在挂载到 {drive}")
                    ok = mount(drive, self.cfg.mount.host, self.cfg.mount.port,
                               self.cfg.mount.dav_user, self.cfg.mount.dav_password)
                    if not ok or not wait_mount(drive):
                        server.stop()
                        raise MountError("挂载失败，请检查 WebClient 服务和端口")
                self._server, self._client = server, client
                self._set("mounted", f"已挂载到 {drive}" if drive else "WebDAV 服务已启动")
            except MountError as exc:
                self._set("error", error=exc.message)
                raise
            except MCloudError as exc:
                self._set("error", error=exc.message)
                raise MountError(exc.message) from exc

    def stop(self) -> None:
        """卸载盘符并停止 WebDAV 服务。

        无论中途是否出错，最终状态都会离开 stopping：
        - 卸载成功：idle。
        - 盘符仍然存在：保持 mounted（WebDAV 服务不关闭，盘符仍可用），并给出错误提示，
          用户关闭占用程序后可以再次卸载。
        """
        with self._lock:
            self._set("stopping", "正在卸载")
            drive = self.cfg.mount.drive
            error = ""
            try:
                if drive and mount_status(drive):
                    if not umount(drive):
                        error = f"盘符 {drive} 未能卸载，可能有程序正在使用它。关闭相关窗口后重试。"
            except Exception as exc:  # noqa: BLE001
                log.exception("卸载盘符时出错")
                error = f"卸载盘符时出错：{exc}"
            if error:
                # 盘符还在：保留服务，状态回到 mounted，错误信息单独展示
                self._set("mounted", f"已挂载到 {drive}", error=error)
                log.warning(error)
                return
            try:
                if self._server is not None:
                    self._server.stop()
            except Exception:  # noqa: BLE001
                log.exception("停止 WebDAV 服务时出错")
            finally:
                self._server = None
                self._client = None
            self._set("idle", "已卸载")

    def check_health(self) -> None:
        """周期调用：如果服务意外退出，更新状态。"""
        if self.state.phase == "mounted" and not self.server_running():
            self._set("error", error="WebDAV 服务意外退出，请重新挂载")

    def reload_config(self) -> None:
        self.cfg = Config.load()
