"""本地配置与凭据存储。

配置文件默认位于 %APPDATA%\\mCloudMount\\config.json。
只存本机使用所需的最小信息：账号、令牌、服务地址、挂载参数。
"""

from __future__ import annotations

import json
import logging
import os
import tempfile
from dataclasses import dataclass, field, asdict
from pathlib import Path
from typing import Any

log = logging.getLogger(__name__)


def config_dir() -> Path:
    base = os.environ.get("MCLOUDMOUNT_HOME")
    if base:
        return Path(base)
    appdata = os.environ.get("APPDATA") or str(Path.home())
    return Path(appdata) / "mCloudMount"


def config_path() -> Path:
    return config_dir() / "config.json"


@dataclass
class MountSettings:
    drive: str = "Z:"
    port: int = 8380
    host: str = "127.0.0.1"
    dav_user: str = "mcloud"
    dav_password: str = "mcloud"


@dataclass
class Account:
    phone: str = ""
    token: str = ""
    token_expire_ms: int = 0
    refresh_token: str = ""
    user_id: str = ""
    device_id: str = ""
    login_id: str = ""
    account: str = ""
    # 服务端在登录响应里下发的地址信息（serverinfo）
    serverinfo: dict[str, Any] = field(default_factory=dict)
    ext_info: dict[str, Any] = field(default_factory=dict)


@dataclass
class Config:
    account: Account = field(default_factory=Account)
    mount: MountSettings = field(default_factory=MountSettings)
    api_host: str = ""          # 覆盖默认 API 域名，便于切测试/生产环境
    env: str = "prod"           # prod / test / dev
    verify_ssl: bool = True
    log_level: str = "INFO"

    # ---------------------------------------------------------------- 读写
    @classmethod
    def load(cls) -> "Config":
        path = config_path()
        if not path.exists():
            return cls()
        try:
            raw = json.loads(path.read_text("utf-8"))
            if not isinstance(raw, dict):
                raise ValueError("config root is not an object")
        except (OSError, ValueError) as exc:
            # 损坏时先备份，避免之后 save() 把登录态覆盖丢失
            backup = path.with_name(path.name + ".bad")
            try:
                os.replace(path, backup)
            except OSError:
                backup = None
            log.warning("配置文件损坏（%s），已备份到 %s，请重新登录", exc, backup or path)
            return cls()
        cfg = cls()
        acct = raw.get("account") or {}
        for key, value in acct.items():
            if hasattr(cfg.account, key):
                setattr(cfg.account, key, value)
        mnt = raw.get("mount") or {}
        for key, value in mnt.items():
            if hasattr(cfg.mount, key):
                setattr(cfg.mount, key, value)
        for key in ("api_host", "env", "verify_ssl", "log_level"):
            if key in raw:
                setattr(cfg, key, raw[key])
        return cfg

    def save(self) -> None:
        path = config_path()
        path.parent.mkdir(parents=True, exist_ok=True)
        payload = {
            "account": asdict(self.account),
            "mount": asdict(self.mount),
            "api_host": self.api_host,
            "env": self.env,
            "verify_ssl": self.verify_ssl,
            "log_level": self.log_level,
        }
        fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=".config-", suffix=".json")
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                json.dump(payload, fh, ensure_ascii=False, indent=2)
            os.replace(tmp, path)
        finally:
            if os.path.exists(tmp):
                try:
                    os.unlink(tmp)
                except OSError:
                    pass
        _harden_acl(path)


def _harden_acl(path: Path) -> None:
    """尽量限制配置文件只对当前用户可读（失败不影响主流程）。"""
    if os.name != "nt":
        try:
            os.chmod(path, 0o600)
        except OSError:
            pass
        return
    import subprocess

    user = os.environ.get("USERNAME")
    if not user:
        return
    try:
        subprocess.run(
            ["icacls", str(path), "/inheritance:r", "/grant:r", f"{user}:F"],
            check=False,
            capture_output=True,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
        )
    except OSError:
        pass
