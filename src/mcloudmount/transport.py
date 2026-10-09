"""UserDomain transport encryption and Android-app request headers."""

from __future__ import annotations

import base64
import binascii
import hashlib
import json
import os
import secrets
import string
import uuid
from typing import Any

import requests
from Crypto.Cipher import AES
from Crypto.Util.Padding import pad, unpad

API_KEY_RELEASE = "2olBaQGYnEKoStYomsd1n7ax"
API_KEY_DEBUG = "PVGDwmcvfs1uV3d1"
LOGIN_KEY_RELEASE = "gB^gGdQHX1l+XteY"
LOGIN_KEY_DEBUG = "QWrZexZTTawO8+I^"
USER_DOMAIN_FALLBACK_KEY = "qPqDw263XgFgL3u8"

USER_DOMAIN_BASE = "https://user-njs.yun.139.com"
DEFAULT_CLIENT_TYPE = "414"
DEFAULT_APP_CHANNEL = "10000023"
DEFAULT_DEVICE_MODEL = "PCKT00"
DEFAULT_DEVICE_MANUFACTURER = "HUAWEI"
DEFAULT_ANDROID_RELEASE = "12"
DEFAULT_ANDROID_SDK = "31"
APP_VERSION = "13.2.4"
APP_VERSION_CODE = "30132408"
DEFAULT_REQ_TYPE = "3"
APP_RELEASE = "0"


def shift(value: str) -> str:
    if len(value) <= 10:
        return value
    return value[-10:] + value[:-10]


def unshift(value: str) -> str:
    if len(value) <= 10:
        return value
    return value[10:] + value[:10]


def encrypt_api_cbc(text: str) -> str:
    iv = os.urandom(AES.block_size)
    cipher = AES.new(API_KEY_RELEASE.encode("ascii"), AES.MODE_CBC, iv)
    encrypted = cipher.encrypt(pad(text.encode("utf-8"), AES.block_size))
    return base64.b64encode(iv + encrypted).decode("ascii")


def decrypt_api_cbc(text: str) -> str | None:
    if not isinstance(text, str):
        return None
    compact = "".join(text.split())
    stripped = compact.rstrip("=")
    candidates = [compact, stripped + "=" * ((4 - len(stripped) % 4) % 4), stripped]
    for candidate in dict.fromkeys(candidates):
        try:
            raw = base64.b64decode(candidate, validate=True)
            if len(raw) <= AES.block_size or len(raw) % AES.block_size:
                continue
            cipher = AES.new(API_KEY_RELEASE.encode("ascii"), AES.MODE_CBC, raw[:AES.block_size])
            return unpad(cipher.decrypt(raw[AES.block_size:]), AES.block_size).decode("utf-8")
        except (binascii.Error, ValueError, UnicodeError):
            continue
    return None


def encrypt_login_ecb(text: str) -> str:
    cipher = AES.new(LOGIN_KEY_RELEASE.encode("ascii"), AES.MODE_ECB)
    encrypted = cipher.encrypt(pad(text.encode("utf-8"), AES.block_size))
    return shift(encrypted.hex().lower())


def decrypt_login_ecb(text: str) -> str | None:
    try:
        raw = bytes.fromhex(unshift(text))
        if not raw or len(raw) % AES.block_size:
            return None
        cipher = AES.new(LOGIN_KEY_RELEASE.encode("ascii"), AES.MODE_ECB)
        return unpad(cipher.decrypt(raw), AES.block_size).decode("utf-8")
    except (ValueError, UnicodeError):
        return None


def decrypt_user_domain_fallback(text: str) -> str | None:
    try:
        raw = bytes.fromhex(text)
        if not raw or len(raw) % AES.block_size:
            return None
        cipher = AES.new(USER_DOMAIN_FALLBACK_KEY.encode("ascii"), AES.MODE_CBC)
        return unpad(cipher.decrypt(raw), AES.block_size).decode("utf-8")
    except (ValueError, UnicodeError):
        return None


def decrypt_login_response(data: str) -> str | None:
    """Apply the APK order: D.dd first, then the old AES-CBC fallback."""
    return decrypt_login_ecb(data) or decrypt_user_domain_fallback(data)


def encrypt_payload(text: str) -> str:
    return encrypt_api_cbc(text)


def decrypt_payload(text: str) -> str | None:
    return decrypt_api_cbc(text)


def prepare_request_json(payload: dict[str, Any], *, login: bool = False) -> str:
    text = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
    return encrypt_login_ecb(text) if login else encrypt_api_cbc(text)


def json_dumps(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def parse_response_json(text: str) -> dict[str, Any]:
    try:
        data = json.loads(text)
        return data if isinstance(data, dict) else {}
    except ValueError:
        return {}


def random_token(size: int = 16) -> str:
    return uuid.uuid4().hex[:size]


def sms_random() -> str:
    """Restore the login UI's six lowercase-letter random value."""
    return "".join(secrets.choice(string.ascii_lowercase) for _ in range(6))


def md5_hex(text: str) -> str:
    return hashlib.md5(text.encode("utf-8")).hexdigest()


def device_info() -> str:
    return (
        f"1|10.0.2.15|1|10.1.1|Android|{DEFAULT_DEVICE_MODEL}|"
        f"{DEFAULT_DEVICE_MANUFACTURER}|{DEFAULT_ANDROID_SDK}|{APP_VERSION}|1|"
    )


def user_agent() -> str:
    return (
        f"android|{DEFAULT_DEVICE_MODEL}|android {DEFAULT_ANDROID_RELEASE}|"
        f"{APP_VERSION}-{APP_RELEASE}"
    )


def auth_header(phone: str, token: str) -> str:
    raw = f"mobile:{phone}:{token}".encode("utf-8")
    return "Basic " + base64.b64encode(raw).decode("ascii")


def common_headers(phone: str = "", token: str = "") -> dict[str, str]:
    info = device_info()
    headers = {
        "x-yun-net-type": "1",
        "x-yun-user-agent": user_agent(),
        "Connection": "keep-alive",
        "x-NetType": "1",
        "x-DeviceInfo": info,
        "x-yun-client-info": info,
        "x-yun-app-channel": DEFAULT_APP_CHANNEL,
        "x-huawei-channelSrc": DEFAULT_APP_CHANNEL,
        "Content-Type": "application/json; charset=utf-8",
        "x-MM-Source": APP_RELEASE,
        "Accept-Language": "zh-CN",
        "x-SvcType": "1",
        "User-Agent": f"okhttp/{APP_VERSION}",
    }
    if phone and token:
        headers["Authorization"] = auth_header(phone, token)
    return headers


def personal_headers(phone: str = "", token: str = "", api_version: str = "v2") -> dict[str, str]:
    info = device_info()
    headers = {
        "x-yun-api-version": api_version,
        "Connection": "keep-alive",
        "x-yun-net-type": "1",
        "x-yun-client-info": info,
        "x-yun-svc-type": "1",
        "x-yun-module-type": "100",
        "x-yun-device-id": info,
        "x-yun-user-agent": user_agent(),
        "x-yun-app-channel": DEFAULT_APP_CHANNEL,
        "x-yun-tid": str(uuid.uuid4()),
        "Accept-Language": "zh-CN",
        "User-Agent": f"okhttp/{APP_VERSION}",
    }
    if phone and token:
        headers["Authorization"] = auth_header(phone, token)
    return headers


def default_http_headers() -> dict[str, str]:
    return {
        "User-Agent": f"okhttp/{APP_VERSION}",
        "Platform": "Android",
    }


def post_json(
    session: requests.Session,
    url: str,
    payload: dict[str, Any],
    headers: dict[str, str],
    timeout: float = 30.0,
    *,
    login: bool = False,
) -> requests.Response:
    body = prepare_request_json(payload, login=login)
    return session.post(url, data=body.encode("utf-8"), headers=headers, timeout=timeout)
