"""开机自启（Windows 当前用户 Run 注册表项）。

非 Windows 环境下所有函数都是安全的空操作，便于在其他系统上测试。
"""

from __future__ import annotations

import os
import sys

RUN_KEY = r"Software\Microsoft\Windows\CurrentVersion\Run"
VALUE_NAME = "mCloudMount"


def _winreg():
    if os.name != "nt":
        return None
    import winreg  # noqa: PLC0415

    return winreg


def launch_command() -> str:
    """开机启动时执行的命令：后台模式启动托盘与面板，不弹出浏览器。"""
    if getattr(sys, "frozen", False):
        return f'"{sys.executable}" --background'
    return f'"{sys.executable}" -m mcloudmount --background'


def is_enabled() -> bool:
    winreg = _winreg()
    if winreg is None:
        return False
    try:
        with winreg.OpenKey(winreg.HKEY_CURRENT_USER, RUN_KEY, 0, winreg.KEY_READ) as key:
            winreg.QueryValueEx(key, VALUE_NAME)
            return True
    except OSError:
        return False


def set_enabled(enabled: bool) -> bool:
    """开启或关闭开机自启。返回最终状态。非 Windows 上返回 False。"""
    winreg = _winreg()
    if winreg is None:
        return False
    if enabled:
        with winreg.CreateKey(winreg.HKEY_CURRENT_USER, RUN_KEY) as key:
            winreg.SetValueEx(key, VALUE_NAME, 0, winreg.REG_SZ, launch_command())
        return True
    try:
        with winreg.OpenKey(winreg.HKEY_CURRENT_USER, RUN_KEY, 0, winreg.KEY_SET_VALUE) as key:
            winreg.DeleteValue(key, VALUE_NAME)
    except OSError:
        pass
    return False
