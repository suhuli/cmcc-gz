"""PC/Web gateway protocol used by the verified 爆米花改造 client.

The Android UserDomain transport uses a different native key. Login requests
instead use AES-256-CBC with an mcloud-sign header over the clear JSON body.
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
from urllib.parse import unquote_to_bytes

from Crypto.Cipher import AES
from Crypto.Util.Padding import pad, unpad

USER_DOMAIN_URL = "https://user-njs.yun.139.com/user"
SMS_URL = f"{USER_DOMAIN_URL}/sms/getSmsCode"
LOGIN_URL = f"{USER_DOMAIN_URL}/thirdlogin"
DEFAULT_BASE_URL = "https://personal-kd-njs.yun.139.com/hcy/"

TRANSPORT_KEY = b"UqEZkrjCKfa02pP6jntzFmkzOz86zHUC"
LOGIN_DATA_KEY = b"qPqDw263XgFgL3u8"
WEB_VERSION = "7.17.9"
WEB_CHANNEL = "10000034"
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


def _md5_hex(value: str) -> str:
    return hashlib.md5(value.encode("utf-8")).hexdigest()


def _encode_uri_component(value: str) -> str:
    encoded = quote(value, safe="")
    replacements = {
        "+": "%20",
        "%21": "!",
        "%27": "'",
        "%28": "(",
        "%29": ")",
        "%7E": "~",
    }
    for old, new in replacements.items():
        encoded = encoded.replace(old, new)
    return encoded


def create_signature(clear_body: str, timestamp: str, nonce: str) -> str:
    sorted_chars = "".join(sorted(_encode_uri_component(clear_body)))
    encoded_base64 = base64.b64encode(sorted_chars.encode("utf-8")).decode("ascii")
    a = _md5_hex(encoded_base64).lower()
    b = _md5_hex(f"{timestamp}:{nonce}").lower()
    return _md5_hex(a + b).upper()


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


def signed_header(clear_body: str) -> str:
    timestamp = format_timestamp()
    nonce = random_nonce()
    return f"{timestamp},{nonce},{create_signature(clear_body, timestamp, nonce)}"


def installation_id(installation_id: str = "") -> str:
    return installation_id or str(secrets.token_hex(16))


def device_info(installation_id: str, language: str = "zh-CN", release: str = "12") -> str:
    return f"||9|{WEB_VERSION}|mobile|Android|{installation_id}||Android {release}||{language}|||"


def common_headers(
    clear_body: str,
    signature: str,
    installation_id_value: str,
    *,
    auth_value: str | None = None,
    network_type: str = "ethernet",
    api_version: str = "v1",
    module_type: str | None = None,
) -> dict[str, str]:
    device = device_info(installation_id_value)
    headers = {
        "Accept": "application/json",
        "Accept-Language": "zh-CN,zh;q=0.9",
        "Connection": "keep-alive",
        "Content-Type": "application/json;charset=UTF-8",
        "hcy-cool-flag": "1",
        "x-NationCode": "+86",
        "CMS-DEVICE": "default",
        "X-Deviceinfo": device,
        "x-yun-net-type": network_type,
        "x-NetType": network_type,
        "x-yun-channel-source": WEB_CHANNEL,
        "x-huawei-channelSrc": WEB_CHANNEL,
        "x-yun-svc-type": "1",
        "x-SvcType": "1",
        "x-m4c-caller": "PC",
        "x-m4c-src": "10002",
        "x-inner-ntwk": "2",
        "mcloud-route": "001",
        "mcloud-version": WEB_VERSION,
        "mcloud-channel": "1000101",
        "mcloud-client": "10701",
        "mcloud-sign": signature,
        "INNER-HCY-ROUTER-HTTPS": "1",
        "x-yun-app-channel": WEB_CHANNEL,
        "x-yun-client-info": f"{device}{base64.b64encode(b'mobile').decode('ascii')}||",
        "x-yun-api-version": api_version,
        "User-Agent": (
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
            "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
        ),
    }
    if auth_value:
        headers["Authorization"] = auth_value
    if module_type:
        headers["x-yun-module-type"] = module_type
    return headers


def basic_auth(kind: str, account: str, token: str) -> str:
    raw = f"{kind}:{account}:{token}".encode("utf-8")
    return "Basic " + base64.b64encode(raw).decode("ascii")


def parse_json_object(value: str) -> dict:
    parsed = json.loads(value)
    return parsed if isinstance(parsed, dict) else {}
