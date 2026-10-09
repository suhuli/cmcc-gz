"""PC/Web 网关协议：登录请求加解密与 mcloud-sign 签名。

登录链路使用 AES-256-CBC 传输层 + 对明文 JSON 的 mcloud-sign 签名。
本模块只包含登录与签名相关的纯函数，便于单元测试。
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import secrets
import string
from dataclasses import dataclass
from datetime import datetime
from urllib.parse import quote

from Crypto.Cipher import AES
from Crypto.Util.Padding import pad, unpad

USER_DOMAIN_URL = "https://user-njs.yun.139.com/user"
DEFAULT_BASE_URL = "https://personal-kd-njs.yun.139.com/hcy/"

TRANSPORT_KEY = b"UqEZkrjCKfa02pP6jntzFmkzOz86zHUC"
LOGIN_DATA_KEY = b"qPqDw263XgFgL3u8"
RANDOM_CHARS = string.ascii_letters + string.digits


@dataclass(frozen=True)
class WireRequest:
    body: str
    signature: str


def compact_json(value: dict) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def build_sms_request(phone: str, random_value: str | None = None) -> str:
    return compact_json(
        {
            "phoneNumber": phone,
            "reqType": "3",
            "random": random_value if random_value is not None else str(int(datetime.now().timestamp() * 1000)),
            "nationCode": "+86",
            "clientType": "414",
            "mode": "0",
        }
    )


def build_login_request(phone: str, sms_code: str) -> str:
    return compact_json(
        {
            "msisdn": phone,
            "clienttype": "414",
            "dycpwd": sms_code,
            "pintype": "8",
            "version": "30131116",
            "cpid": "58",
            "loginMode": "0",
            "extInfo": {},
        }
    )


def encrypt_transport(clear_text: str, iv: bytes | None = None) -> str:
    iv = os.urandom(16) if iv is None else iv
    if len(iv) != 16:
        raise ValueError("IV must be 16 bytes")
    cipher = AES.new(TRANSPORT_KEY, AES.MODE_CBC, iv)
    encrypted = cipher.encrypt(pad(clear_text.encode("utf-8"), AES.block_size))
    return base64.b64encode(iv + encrypted).decode("ascii")


def decrypt_transport(raw_text: str) -> str:
    text = raw_text.strip().lstrip("\ufeff")
    if text.startswith("{") or text.startswith("["):
        return text
    padding = "=" * ((4 - len(text) % 4) % 4)
    packet = base64.b64decode(text + padding)
    if len(packet) <= 16:
        raise ValueError("encrypted response is too short")
    cipher = AES.new(TRANSPORT_KEY, AES.MODE_CBC, packet[:16])
    return unpad(cipher.decrypt(packet[16:]), AES.block_size).decode("utf-8")


def decrypt_login_data(cipher_hex: str) -> str:
    cipher = AES.new(LOGIN_DATA_KEY, AES.MODE_ECB)
    plain = unpad(cipher.decrypt(bytes.fromhex(cipher_hex)), AES.block_size)
    return plain.decode("utf-8")


def md5_hex(value: str) -> str:
    return hashlib.md5(value.encode("utf-8")).hexdigest()


def encode_uri_component(value: str) -> str:
    """模拟 JavaScript encodeURIComponent 的编码结果。"""
    encoded = quote(value, safe="")
    for old, new in {
        "+": "%20",
        "%21": "!",
        "%27": "'",
        "%28": "(",
        "%29": ")",
        "%7E": "~",
    }.items():
        encoded = encoded.replace(old, new)
    return encoded


def create_signature(clear_body: str, timestamp: str, nonce: str) -> str:
    sorted_chars = "".join(sorted(encode_uri_component(clear_body)))
    encoded_base64 = base64.b64encode(sorted_chars.encode("utf-8")).decode("ascii")
    a = md5_hex(encoded_base64).lower()
    b = md5_hex(f"{timestamp}:{nonce}").lower()
    return md5_hex(a + b).upper()


def random_nonce() -> str:
    return "".join(secrets.choice(RANDOM_CHARS) for _ in range(16))


def format_timestamp(now: datetime | None = None) -> str:
    return (now or datetime.now()).strftime("%Y%m%d %H%M%S")


def prepare_request(clear_body: str) -> WireRequest:
    timestamp = format_timestamp()
    nonce = random_nonce()
    return WireRequest(
        body=encrypt_transport(clear_body),
        signature=f"{timestamp},{nonce},{create_signature(clear_body, timestamp, nonce)}",
    )


def basic_auth(kind: str, account: str, token: str) -> str:
    raw = f"{kind}:{account}:{token}".encode("utf-8")
    return "Basic " + base64.b64encode(raw).decode("ascii")
