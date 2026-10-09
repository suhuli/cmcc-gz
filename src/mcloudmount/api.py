"""中国移动云盘协议客户端。

登录链路：`user/sms/getSmsCode` -> `user/thirdlogin`。
文件链路：`file/list`、`file/create`、`file/getDownloadUrl`、
`file/getUploadUrl`、`file/listUploadedParts`、`file/complete`。
"""

from __future__ import annotations

import base64
import hashlib
import json
import random
import string
import time
import uuid
from datetime import datetime
from typing import Any, Iterator
from urllib.parse import quote, urljoin
import urllib.request

import requests
from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry

from .config import Account, Config
from .errors import ApiError, AuthError, ConflictError, NetworkError, NotFoundError, RateLimitedError
from .pc_login import (
    DEFAULT_BASE_URL,
    USER_DOMAIN_URL as PC_USER_DOMAIN_URL,
    basic_auth,
    build_login_request,
    build_sms_request,
    compact_json,
    decrypt_login_data,
    decrypt_transport,
    prepare_request,
)
from .transport import default_http_headers

DEFAULT_PERSONAL_URL = DEFAULT_BASE_URL
DEFAULT_FILE_TRANSFER_URL = DEFAULT_BASE_URL
DEFAULT_FILE_TYPE_DIR = "folder"
DEFAULT_FILE_TYPE_FILE = "file"
PC_PROFILE = "pc"
MOBILE_PROFILE = "mobile"
WEB_VERSION = "7.17.9"
WEB_CHANNEL = "10000034"


def _md5_hex(value: str) -> str:
    return hashlib.md5(value.encode("utf-8")).hexdigest()


def _encode_uri_component(value: str) -> str:
    encoded = quote(value, safe="")
    for old, new in {
        "+": "%20", "%21": "!", "%27": "'", "%28": "(",
        "%29": ")", "%7E": "~",
    }.items():
        encoded = encoded.replace(old, new)
    return encoded


def create_signature(clear_body: str, timestamp: str, nonce: str) -> str:
    sorted_chars = "".join(sorted(_encode_uri_component(clear_body)))
    encoded_base64 = base64.b64encode(sorted_chars.encode("utf-8")).decode("ascii")
    a = _md5_hex(encoded_base64).lower()
    b = _md5_hex(f"{timestamp}:{nonce}").lower()
    return _md5_hex(a + b).upper()


def _web_device_info(device_id: str = "mcloudmount") -> str:
    return f"||9|{WEB_VERSION}|mobile|Android|{device_id}||Android 14||zh-CN|||"


def _web_client_info(device_id: str = "mcloudmount") -> str:
    return _web_device_info(device_id) + base64.b64encode(b"mobile").decode("ascii") + "||"


def _signed_header(clear_body: str) -> str:
    timestamp = datetime.now().strftime("%Y%m%d %H%M%S")
    nonce = "".join(random.choice(string.ascii_letters + string.digits) for _ in range(16))
    return f"{timestamp},{nonce},{create_signature(clear_body, timestamp, nonce)}"


def file_headers(
    profile: str,
    phone: str,
    token: str,
    clear_body: str,
    device_id: str = "mcloudmount",
    api_version: str = "v1",
) -> dict[str, str]:
    auth = basic_auth(profile, phone, token)
    device = _web_device_info(device_id)
    return {
        "Authorization": auth,
        "Accept": "application/json",
        "Accept-Language": "zh-CN",
        "Connection": "keep-alive",
        "Content-Type": "application/json; charset=UTF-8",
        "CMS-DEVICE": "default",
        "X-Deviceinfo": device,
        "x-yun-net-type": "wifi",
        "x-NetType": "wifi",
        "x-yun-client-info": _web_client_info(device_id),
        "x-yun-svc-type": "1",
        "x-SvcType": "1",
        "x-yun-module-type": "100",
        "x-yun-app-channel": WEB_CHANNEL,
        "x-yun-channel-source": WEB_CHANNEL,
        "x-huawei-channelSrc": WEB_CHANNEL,
        "x-m4c-caller": "PC",
        "x-m4c-src": "10002",
        "x-inner-ntwk": "2",
        "mcloud-route": "001",
        "mcloud-version": WEB_VERSION,
        "mcloud-channel": "1000101",
        "mcloud-client": "10701",
        "mcloud-sign": _signed_header(clear_body),
        "INNER-HCY-ROUTER-HTTPS": "1",
        "x-yun-api-version": api_version,
    }


def _user_domain_headers(clear_body: str, phone: str) -> dict[str, str]:
    device_id = f"web-{phone[-4:]}" if len(phone) >= 4 else "web-0000"
    device = _web_device_info(device_id)
    return {
        "Accept": "application/json",
        "Accept-Language": "zh-CN",
        "Connection": "keep-alive",
        "Content-Type": "application/json;charset=UTF-8",
        "hcy-cool-flag": "1",
        "x-NationCode": "+86",
        "CMS-DEVICE": "default",
        "X-Deviceinfo": device,
        "x-yun-net-type": "wifi",
        "x-NetType": "wifi",
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
        "mcloud-sign": _signed_header(clear_body),
        "INNER-HCY-ROUTER-HTTPS": "1",
        "x-yun-app-channel": WEB_CHANNEL,
        "x-yun-client-info": _web_client_info(device_id),
        "x-yun-api-version": "v1",
    }


def _parse_json_object(raw: str) -> dict[str, Any]:
    value = json.loads(raw)
    return value if isinstance(value, dict) else {}


def _decode_login_data(data: Any) -> dict[str, Any]:
    if isinstance(data, dict):
        return data
    if not isinstance(data, str) or not data.strip():
        raise ApiError("登录响应没有授权数据")
    value = data.strip()
    if not value.startswith("{"):
        value = decrypt_login_data(value)
    return _parse_json_object(value)


class MCloudClient:
    """登录与文件域协议封装。"""

    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.session = requests.Session()
        self.session.verify = cfg.verify_ssl
        retry = Retry(
            total=3,
            connect=3,
            read=2,
            backoff_factor=0.3,
            status_forcelist=(429, 500, 502, 503, 504),
            allowed_methods=frozenset({"GET", "HEAD", "OPTIONS", "PUT"}),
            respect_retry_after_header=True,
        )
        adapter = HTTPAdapter(max_retries=retry, pool_connections=8, pool_maxsize=16)
        self.session.mount("http://", adapter)
        self.session.mount("https://", adapter)
        self.user_domain_url = cfg.api_host or PC_USER_DOMAIN_URL
        self.personal_url = DEFAULT_PERSONAL_URL
        self._apply_login_urls(cfg.account.serverinfo)

    def _apply_login_urls(self, serverinfo: dict[str, Any]) -> None:
        router = serverinfo.get("routerInfo") if isinstance(serverinfo, dict) else []
        if not isinstance(router, list):
            return
        for item in router:
            if not isinstance(item, dict):
                continue
            name = str(item.get("modName") or item.get("name") or "").strip()
            url = str(item.get("httpsUrl") or item.get("httpUrl") or "").strip()
            if name == "personal" and url.startswith("https://"):
                self.personal_url = url if url.endswith("/") else url + "/"

    def _require_token(self) -> None:
        account = self.cfg.account
        if not account.phone or not account.token:
            raise AuthError("请先完成短信登录")
        if account.token_expire_ms and account.token_expire_ms <= time.time() * 1000:
            raise AuthError("令牌已过期，请重新登录")

    def send_sms_code(self, phone: str, req_type: str = "3") -> dict[str, Any]:
        if not self._valid_phone(phone):
            raise AuthError("请输入 11 位中国大陆手机号")
        clear = build_sms_request(phone, str(int(datetime.now().timestamp() * 1000)))
        return self._post_user_domain_clear("sms/getSmsCode", clear, phone)

    def probe_login_request(self, phone: str, sms_code: str = "123456") -> dict[str, Any]:
        clear = build_login_request(phone, sms_code)
        wire = prepare_request(clear)
        headers = _user_domain_headers(clear, phone)
        headers["mcloud-sign"] = wire.signature
        return {
            "url": self.user_domain_url.rstrip("/") + "/thirdlogin",
            "payload": _parse_json_object(clear),
            "wire": wire.body,
            "headers": headers,
        }

    def login_with_sms(self, phone: str, sms_code: str) -> dict[str, Any]:
        if not self._valid_phone(phone):
            raise AuthError("请输入 11 位中国大陆手机号")
        if not (sms_code.isdigit() and 4 <= len(sms_code) <= 8):
            raise AuthError("请输入正确的短信验证码")
        clear = build_login_request(phone, sms_code)
        result = self._post_user_domain_clear("thirdlogin", clear, phone)
        if "account" not in result:
            result["account"] = phone
        return result

    @staticmethod
    def _valid_phone(phone: str) -> bool:
        return len(phone) == 11 and phone.startswith("1") and phone.isdigit()

    def _post_user_domain_clear(self, path: str, clear: str, phone: str) -> dict[str, Any]:
        url = urljoin(self.user_domain_url.rstrip("/") + "/", path.lstrip("/"))
        wire = prepare_request(clear)
        headers = _user_domain_headers(clear, phone)
        headers["mcloud-sign"] = wire.signature
        try:
            resp = self.session.post(url, data=wire.body.encode("utf-8"), headers=headers, timeout=30.0)
        except requests.RequestException as exc:
            raise NetworkError(f"网络请求失败: {exc}") from exc
        text = resp.text.strip()
        if not resp.ok or not text:
            raise ApiError(
                f"登录服务 HTTP 请求失败: {resp.status_code} {text[:200]}",
                http_status=resp.status_code,
            )
        try:
            envelope = _parse_json_object(decrypt_transport(text))
        except (ValueError, UnicodeError) as exc:
            raise ApiError(
                f"登录响应解密失败: {resp.status_code} {text[:200]}",
                http_status=resp.status_code,
            ) from exc
        code = str(envelope.get("code") or "")
        success = envelope.get("success") is True or code in ("0", "0000")
        if not success:
            raise ApiError(
                str(envelope.get("message") or envelope.get("desc") or "登录服务返回失败"),
                code=code or None,
                http_status=resp.status_code,
            )
        if "data" not in envelope:
            return {"return": code, "envelope": envelope}
        try:
            login_data = _decode_login_data(envelope.get("data"))
        except ValueError as exc:
            raise ApiError("登录服务返回的授权数据无法解密", http_status=resp.status_code) from exc
        result_code = str(login_data.get("return") or login_data.get("code") or "")
        if result_code not in ("", "0", "0000"):
            raise ApiError(
                str(login_data.get("desc") or login_data.get("message") or "登录服务返回失败"),
                code=result_code,
                http_status=resp.status_code,
            )
        login_data.setdefault("return", result_code or "0")
        return login_data

    def login_result_success(self, result: dict[str, Any]) -> bool:
        code = str(result.get("return", result.get("returnResult", result.get("code", ""))))
        return code in {"0", "0000", "200059525", "200059526", "200059537", "200059538", "200059540"}

    def save_account(self, phone: str, result: dict[str, Any]) -> Account:
        account = self.cfg.account
        account.phone = phone
        account.account = str(result.get("account") or phone)
        account.token = str(result.get("authToken") or result.get("token") or "")
        account.refresh_token = str(result.get("token") or "")
        account.user_id = str(result.get("userid") or result.get("userDomainId") or "")
        account.device_id = str(result.get("deviceId") or uuid.uuid4().hex)
        account.login_id = str(result.get("loginid") or "")
        expire = result.get("atExpiretime")
        if isinstance(expire, (int, float)) and expire > 0:
            now_ms = int(time.time() * 1000)
            if expire > 10**12:
                account.token_expire_ms = int(expire)
            elif expire >= 10**9:
                account.token_expire_ms = int(expire * 1000)
            else:
                account.token_expire_ms = now_ms + int(expire * 1000)
        router = result.get("routerInfo")
        if isinstance(router, list):
            account.serverinfo["routerInfo"] = router
        serverinfo = result.get("serverinfo")
        if isinstance(serverinfo, dict):
            account.serverinfo.update(serverinfo)
        self._apply_login_urls(account.serverinfo)
        self.cfg.save()
        return account

    def resolve_connection(self) -> dict[str, Any]:
        """登录后用只读根目录请求协商 pc/mobile 身份和备用 token。"""
        self._require_token()
        account = self.cfg.account
        base_urls = [self.personal_url, DEFAULT_BASE_URL]
        saved_profile = str(account.ext_info.get("profile") or PC_PROFILE)
        profiles = [saved_profile, PC_PROFILE, MOBILE_PROFILE]
        tokens = [account.token]
        if account.refresh_token and account.refresh_token != account.token:
            tokens.append(account.refresh_token)
        candidates: list[tuple[str, str, str]] = []
        for base_url in dict.fromkeys(base_urls):
            for token in tokens:
                for profile in profiles:
                    candidates.append((base_url, profile, token))
        failures: list[str] = []
        for base_url, profile, token in candidates:
            try:
                data = self.get_disk("/", page_size=1, base_url=base_url,
                                     profile=profile, token=token)
                self.personal_url = base_url
                account.token = token
                account.ext_info["profile"] = profile
                self.cfg.save()
                return data
            except Exception as exc:
                detail = getattr(exc, "message", None) or str(exc)
                failures.append(f"{profile}@{base_url}: {detail[:120]}")
        raise ApiError("文件协议协商失败 [" + "; ".join(failures[-8:]) + "]")

    def get_disk(
        self,
        parent_file_id: str = "/",
        *,
        page_size: int = 200,
        page_cursor: str = "",
        order_by: str = "name",
        order_direction: str = "ASC",
        type_filter: str | None = None,
        base_url: str | None = None,
        profile: str | None = None,
        token: str | None = None,
    ) -> dict[str, Any]:
        self._require_token()
        account = self.cfg.account
        base_url = base_url or self.personal_url
        profile = profile or str(account.ext_info.get("profile") or PC_PROFILE)
        token = token or account.token
        payload: dict[str, Any] = {
            "parentFileId": parent_file_id,
            "parentFilePath": False,
            "pageInfo": {
                "pageSize": page_size,
                "pageCursor": page_cursor,
                "needTotalCount": 0,
            },
            "orderBy": order_by,
            "orderDirection": order_direction,
            "workSpaceType": "",
            "fields": "thumbnailUrls,addressDetail,mediaMetaInfo,metadataAuditInfo,userTags,contentAuditInfo,starredAt,starred,localCreatedAt,localUpdatedAt",
            "imageThumbnailStyleList": ["Small", "Large"],
        }
        if type_filter:
            payload["type"] = type_filter
        clear = compact_json(payload)
        headers = file_headers(
            profile,
            account.account or account.phone,
            token,
            clear,
            account.device_id or f"web-{(account.phone or '0000')[-4:]}",
        )
        url = urljoin(base_url, "file/list")
        try:
            resp = self.session.post(
                url,
                data=clear.encode("utf-8"),
                headers=headers,
                timeout=30.0,
            )
        except requests.RequestException as exc:
            raise NetworkError(f"文件列表请求失败: {exc}") from exc
        data = self._parse_plain_json(resp)
        self._ensure_ok(data)
        return data

    def iter_folder_items(
        self,
        parent_file_id: str = "/",
        *,
        page_size: int = 200,
        order_by: str = "name",
        order_direction: str = "ASC",
        type_filter: str | None = None,
    ) -> Iterator[dict[str, Any]]:
        cursor = ""
        while True:
            data = self.get_disk(
                parent_file_id,
                page_size=page_size,
                page_cursor=cursor,
                order_by=order_by,
                order_direction=order_direction,
                type_filter=type_filter,
            )
            node = data.get("data") or {}
            for item in node.get("items") or []:
                yield item
            previous = cursor
            cursor = str(node.get("nextPageCursor") or "")
            if cursor and cursor == previous:
                raise ApiError("文件列表分页游标没有推进")
            if not cursor:
                break

    def create_folder(
        self,
        parent_file_id: str,
        name: str,
        *,
        file_rename_mode: str | None = None,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "parentFileId": parent_file_id,
            "name": name,
            "type": DEFAULT_FILE_TYPE_DIR,
        }
        data = self._post_personal("file/create", payload)
        self._ensure_ok(data)
        return data

    def rename(self, file_id: str, new_name: str) -> dict[str, Any]:
        payload = {
            "fileId": file_id,
            "name": new_name,
            "FileRenameMode": "refuse",
        }
        data = self._post_personal("file/update", payload, api_version="v1")
        self._ensure_ok(data)
        return data

    def move(self, file_ids: list[str], to_parent_file_id: str) -> dict[str, Any]:
        payload = {
            "fileIds": file_ids,
            "toParentFileId": to_parent_file_id,
        }
        data = self._post_personal("file/batchMove", payload)
        self._ensure_ok(data)
        return data

    def trash(self, file_ids: list[str]) -> dict[str, Any]:
        payload = {"fileIds": file_ids}
        data = self._post_personal("recyclebin/batchTrash", payload, api_version="v1")
        self._ensure_ok(data)
        return data

    def get_download_url(self, file_id: str, expire_sec: int = 3600) -> dict[str, Any]:
        payload = {
            "fileId": file_id,
            "userId": self.cfg.account.user_id,
        }
        data = self._post_personal("file/getDownloadUrl", payload)
        self._ensure_ok(data)
        return data

    def upload_part(self, upload_url: str, data: Any, length: int | None = None, *, timeout: float = 180.0):
        if hasattr(data, "read"):
            chunks = []
            remaining = length
            while remaining is None or remaining > 0:
                size = 1024 * 1024 if remaining is None else min(1024 * 1024, remaining)
                chunk = data.read(size)
                if not chunk:
                    break
                chunks.append(chunk)
                if remaining is not None:
                    remaining -= len(chunk)
            data = b"".join(chunks)
        headers = {"Content-Length": str(len(data))}
        request = urllib.request.Request(upload_url, data=data, method="PUT", headers=headers)
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                result = requests.Response()
                result.status_code = response.status
                result.headers = {k.lower(): v for k, v in response.headers.items()}
                result._content = b""
                return result
        except Exception as exc:
            raise NetworkError(f"上传分片失败: {exc}") from exc

    def complete_upload(
        self,
        file_id: str,
        upload_id: str = "",
        transfer_token: str = "",
        content_hash: str = "",
    ) -> dict[str, Any]:
        payload = {
            "fileId": file_id,
            "uploadId": upload_id,
            "contentHash": content_hash,
            "contentHashAlgorithm": "SHA256",
            # 实测后端拒绝空字符串，允许省略该字段。
            **({"transferToken": transfer_token} if transfer_token else {}),
        }
        return self._post_personal("file/complete", payload)

    def list_uploaded_parts(
        self,
        file_id: str,
        upload_id: str = "",
        transfer_token: str = "",
    ) -> dict[str, Any]:
        payload = {
            "fileId": file_id,
            "uploadId": upload_id,
            "contentHash": "",
            "contentHashAlgorithm": "SHA256",
            "transferToken": transfer_token,
        }
        return self._post_personal("file/listUploadedParts", payload)

    def _post_personal(
        self,
        path: str,
        payload: dict[str, Any],
        *,
        api_version: str = "v1",
        timeout: float = 30.0,
    ) -> dict[str, Any]:
        self._require_token()
        account = self.cfg.account
        profile = str(account.ext_info.get("profile") or PC_PROFILE)
        headers = file_headers(
            profile,
            account.account or account.phone,
            account.token,
            compact_json(payload),
            account.device_id or f"web-{(account.phone or '0000')[-4:]}",
            api_version,
        )
        url = urljoin(self.personal_url, path.lstrip("/"))
        try:
            resp = self.session.post(
                url,
                data=compact_json(payload).encode("utf-8"),
                headers=headers,
                timeout=timeout,
            )
        except requests.RequestException as exc:
            raise NetworkError(f"文件请求失败: {exc}") from exc
        data = self._parse_plain_json(resp)
        self._ensure_ok(data)
        return data

    def _parse_plain_json(self, resp: requests.Response) -> dict[str, Any]:
        try:
            data = resp.json()
            if isinstance(data, dict):
                return data
            return {}
        except ValueError:
            raise ApiError(f"响应不是合法 JSON: {resp.status_code} {resp.text[:200]}", http_status=resp.status_code)

    def _ensure_ok(self, data: dict[str, Any]) -> None:
        code = data.get("code", data.get("return"))
        success = data.get("success")
        if success is True or str(code) in ("0", "0000", "200", "SUCCESS", ""):
            return
        message = str(data.get("message") or data.get("desc") or "服务端返回错误")
        error = ApiError(message, code=code, http_status=None)
        code_text = str(code)
        if code_text in ("401", "403", "200000401", "200000413"):
            raise AuthError(message, code=code)
        if code_text in ("404", "200000404"):
            raise NotFoundError(message, code=code)
        if code_text in ("409", "200000409"):
            raise ConflictError(message, code=code)
        if code_text in ("429", "200000429"):
            raise RateLimitedError(message, code=code)
        raise error

    def download_stream(
        self, url: str, *, timeout: float = 30.0, headers: dict[str, str] | None = None
    ) -> requests.Response:
        headers = headers or default_http_headers()
        try:
            return self.session.get(url, headers=headers, stream=True, timeout=timeout, allow_redirects=True)
        except requests.RequestException as exc:
            raise NetworkError(f"下载失败: {exc}") from exc
    def upload_create(
        self,
        parent_file_id: str,
        name: str,
        size: int,
        *,
        content_hash: str = "",
        content_hash_algorithm: str = "",
        file_rename_mode: str | None = None,
        part_infos: list[dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "parentFileId": parent_file_id,
            "name": name,
            "size": size,
            "type": DEFAULT_FILE_TYPE_FILE,
            "contentType": "application/octet-stream",
            "contentHash": content_hash,
            "contentHashAlgorithm": content_hash_algorithm,
            "fileRenameMode": file_rename_mode or "auto_rename",
            "parallelUpload": False,
            "partInfos": part_infos or [],
        }
        return self._post_personal("file/create", payload)

    def get_upload_url(
        self,
        file_id: str,
        part_infos: list[dict[str, Any]],
        *,
        upload_id: str = "",
        transfer_token: str = "",
    ) -> dict[str, Any]:
        payload = {
            "fileId": file_id,
            "uploadId": upload_id,
            "partInfos": part_infos,
            # 初始化未下发 token 时，后端拒绝空字符串，必须省略。
            **({"transferToken": transfer_token} if transfer_token else {}),
            "userRegion": {},
        }
        return self._post_personal("file/getUploadUrl", payload)
