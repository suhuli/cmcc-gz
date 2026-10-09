"""系统托盘图标（可选依赖 pystray + Pillow）。

未安装依赖时 is_available() 返回 False，调用方退回到普通面板模式。
"""

from __future__ import annotations

import importlib
import logging
import webbrowser
from typing import Callable

log = logging.getLogger("mcloudmount.tray")

BLUE = (37, 99, 235, 255)
WHITE = (255, 255, 255, 255)


def is_available() -> bool:
    try:
        importlib.import_module("PIL")
        importlib.import_module("pystray")
    except Exception:  # noqa: BLE001
        return False
    return True


def _make_icon_image():
    from PIL import Image, ImageDraw

    size = 64
    img = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    draw = ImageDraw.Draw(img)
    draw.rounded_rectangle((2, 2, size - 2, size - 2), radius=16, fill=BLUE)
    # 简洁的“云”形：三个圆 + 底部矩形
    draw.ellipse((12, 24, 32, 44), fill=WHITE)
    draw.ellipse((22, 16, 44, 40), fill=WHITE)
    draw.ellipse((36, 26, 52, 42), fill=WHITE)
    draw.rounded_rectangle((12, 32, 52, 44), radius=6, fill=WHITE)
    return img


def run_tray(
    url: str,
    *,
    on_toggle_mount: Callable[[], None],
    mounted_label: Callable[[], str],
    on_quit: Callable[[], None],
    on_open_drive: Callable[[], None],
) -> None:
    """在当前线程阻塞运行托盘图标，直到用户选择退出。"""
    import pystray

    def _open(icon, item=None):  # noqa: ARG001
        webbrowser.open(url)

    def _toggle(icon, item=None):  # noqa: ARG001
        try:
            on_toggle_mount()
        except Exception as exc:  # noqa: BLE001
            log.error("托盘操作失败: %s", exc)

    def _drive(icon, item=None):  # noqa: ARG001
        on_open_drive()

    def _quit(icon, item=None):  # noqa: ARG001
        icon.stop()
        on_quit()

    menu = pystray.Menu(
        pystray.MenuItem("打开控制面板", _open, default=True),
        pystray.MenuItem(lambda item: mounted_label(), _toggle),
        pystray.MenuItem("打开云盘所在位置", _drive),
        pystray.Menu.SEPARATOR,
        pystray.MenuItem("退出", _quit),
    )
    icon = pystray.Icon("mcloudmount", _make_icon_image(), "mCloudMount", menu)
    icon.run()
