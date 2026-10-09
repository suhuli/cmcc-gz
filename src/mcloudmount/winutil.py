"""Windows WebDAV 客户端挂载辅助。"""

from __future__ import annotations

import re
import subprocess
import time

def _run(cmd: list[str], *, check: bool = False, timeout: float = 20.0) -> subprocess.CompletedProcess:
    """执行系统命令。命令不存在时返回失败结果；超时也返回失败结果，避免界面永久卡住。"""
    try:
        return subprocess.run(
            cmd,
            check=check,
            capture_output=True,
            text=True,
            timeout=timeout,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
        )
    except subprocess.TimeoutExpired:
        return subprocess.CompletedProcess(cmd, 124, stdout="", stderr=f"命令超时（{timeout:.0f} 秒）")
    except OSError as exc:
        return subprocess.CompletedProcess(cmd, 127, stdout="", stderr=str(exc))


_RUNNING_STATE = re.compile(r"(STATE|状态)\s*:\s*4\b", re.IGNORECASE)


def webclient_running() -> bool:
    """用数字状态码 4（RUNNING）判断，兼容中文等非英文系统的 sc 输出。"""
    state = _run(["sc", "query", "WebClient"])
    return bool(_RUNNING_STATE.search(state.stdout)) or "RUNNING" in state.stdout.upper()


def ensure_webclient() -> bool:
    """启动 WebClient 服务；失败返回 False，由上层提示。"""
    if webclient_running():
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
    """卸载盘符。返回 True 表示盘符已经不存在。

    失败后等待 1 秒重试一次（盘符刚被使用时常常第二次就成功）；
    最终以 `net use` 列表为准判断，而不是只看返回码。
    """
    cmd = ["net", "use", f"{drive}", "/delete", "/yes"]
    _run(cmd)
    if not mount_status(drive):
        return True
    time.sleep(1.0)
    _run(cmd)
    return not mount_status(drive)


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
