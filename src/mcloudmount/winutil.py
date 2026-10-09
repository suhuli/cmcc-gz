"""Windows WebDAV 客户端挂载辅助。"""

from __future__ import annotations

import subprocess
import time

def _run(cmd: list[str], *, check: bool = False) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd,
        check=check,
        capture_output=True,
        text=True,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )


def ensure_webclient() -> bool:
    """启动 WebClient 服务；失败返回 False，由上层提示。"""
    state = _run(["sc", "query", "WebClient"])
    if "RUNNING" in state.stdout:
        return True
    result = _run(["net", "start", "WebClient"])
    return result.returncode == 0


def set_webclient_limits() -> bool:
    """放宽大文件下载限制；失败不阻塞主流程。"""
    base = r"HKLM\SYSTEM\CurrentControlSet\Services\WebClient\Parameters"
    ok1 = _run(
        [
            "reg",
            "add",
            base,
            "/v",
            "FileSizeLimitInBytes",
            "/t",
            "REG_DWORD",
            "/d",
            "4294967295",
            "/f",
        ]
    )
    ok2 = _run(
        [
            "reg",
            "add",
            base,
            "/v",
            "BasicAuthLevel",
            "/t",
            "REG_DWORD",
            "/d",
            "2",
            "/f",
        ]
    )
    return ok1.returncode == 0 and ok2.returncode == 0


def mount(drive: str, host: str, port: int, user: str, password: str) -> bool:
    """将 WebDAV 根目录挂载为盘符。"""
    unc = f"\\\\{host}@{port}\\DavWWWRoot"
    result = _run(
        [
            "net",
            "use",
            f"{drive}",
            unc,
            password,
            f"/user:{user}",
            "/persistent:no",
        ]
    )
    return result.returncode == 0


def umount(drive: str) -> bool:
    result = _run(["net", "use", f"{drive}", "/delete", "/yes"])
    return result.returncode == 0


def mount_status(drive: str) -> bool:
    result = _run(["net", "use"])
    return f"{drive}" in result.stdout


def wait_mount(drive: str, timeout: float = 8.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if mount_status(drive):
            return True
        time.sleep(0.3)
    return False
