"""通用 HTTP 请求头（下载、预览等非 API 请求使用）。

所有 API 协议（登录加密、签名、设备信息）集中在 pc_login.py 与 api.py 中。
"""

from __future__ import annotations

APP_VERSION = "13.2.4"


def default_http_headers() -> dict[str, str]:
    return {
        "User-Agent": f"okhttp/{APP_VERSION}",
        "Platform": "Android",
    }
