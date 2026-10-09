"""统一的异常类型。"""


class MCloudError(Exception):
    """所有云盘客户端错误的基类。"""

    def __init__(self, message: str, *, code: str | int | None = None, http_status: int | None = None):
        super().__init__(message)
        self.message = message
        self.code = code
        self.http_status = http_status

    def __str__(self) -> str:  # pragma: no cover - 调试友好
        parts = [self.message]
        if self.code is not None:
            parts.append(f"code={self.code}")
        if self.http_status is not None:
            parts.append(f"http={self.http_status}")
        return " | ".join(parts)


class NetworkError(MCloudError):
    """网络层失败（超时、连接重置等），可重试。"""


class AuthError(MCloudError):
    """令牌缺失/过期，需要重新登录。"""


class ApiError(MCloudError):
    """服务端返回了业务错误码。"""


class NotFoundError(MCloudError):
    """资源不存在。"""


class ConflictError(MCloudError):
    """重名或状态冲突。"""


class RateLimitedError(MCloudError):
    """被限流，可退避重试。"""
