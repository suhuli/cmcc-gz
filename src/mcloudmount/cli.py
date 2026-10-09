"""命令行入口。"""

from __future__ import annotations

import argparse
import logging
import time

from .api import MCloudClient
from .config import Config
from .davserver import DavServer
from .errors import AuthError, MCloudError
from .winutil import ensure_webclient, mount, mount_status, set_webclient_limits, umount, wait_mount


def _configure_logging(level: str) -> None:
    logging.basicConfig(
        level=getattr(logging, level.upper(), logging.INFO),
        format="%(asctime)s %(levelname)s %(message)s",
    )


def _make_client(cfg: Config) -> MCloudClient:
    return MCloudClient(cfg)


def cmd_login(args: argparse.Namespace) -> int:
    cfg = Config.load()
    client = _make_client(cfg)
    phone = args.phone
    if not phone:
        phone = input("Phone number: ").strip()
    if not phone:
        print("Phone number不能为空")
        return 2
    print("正在发送短信验证码...")
    client.send_sms_code(phone)
    print("验证码已发送，请查收短信")
    code = input("短信验证码: ").strip()
    if not code:
        print("短信验证码不能为空")
        return 2
    print("正在登录...")
    login_result = client.login_with_sms(phone, code)
    if not client.login_result_success(login_result):
        print(f"登录失败: {login_result}")
        return 2
    client.save_account(phone, login_result)
    print("登录成功，正在协商文件协议...")
    data = client.resolve_connection()
    profile = data.get("profile") or client.cfg.account.ext_info.get("profile")
    print(f"登录成功，文件协议协商完成: {profile}")
    print(f"个人网盘地址: {client.personal_url}")
    return 0


def cmd_logout(args: argparse.Namespace) -> int:
    cfg = Config.load()
    cfg.account = type(cfg.account)()
    cfg.save()
    print("已清除本地登录状态")
    return 0


def cmd_status(args: argparse.Namespace) -> int:
    cfg = Config.load()
    account = cfg.account
    if not account.phone:
        print("Not logged in")
        return 1
    expired = bool(account.token_expire_ms and account.token_expire_ms <= time.time() * 1000)
    print(f"Account: {account.phone}")
    print(f"Token: {'valid' if not expired else 'expired'}")
    print(f"Drive: {cfg.mount.drive}")
    print(f"WebDAV: http://{cfg.mount.host}:{cfg.mount.port}")
    return 0


def cmd_mount(args: argparse.Namespace) -> int:
    cfg = Config.load()
    if not cfg.account.phone or not cfg.account.token:
        print("请先执行 login")
        return 1
    _configure_logging(cfg.log_level)
    ensure_webclient()
    set_webclient_limits()
    client = _make_client(cfg)
    print("正在验证登录状态...")
    client.resolve_connection()
    if cfg.mount.drive and mount_status(cfg.mount.drive):
        umount(cfg.mount.drive)
    server = DavServer(
        client,
        cfg.mount.host,
        cfg.mount.port,
        cfg.mount.dav_user,
        cfg.mount.dav_password,
    )
    server.start_background()
    if cfg.mount.drive:
        ok = mount(
            cfg.mount.drive,
            cfg.mount.host,
            cfg.mount.port,
            cfg.mount.dav_user,
            cfg.mount.dav_password,
        )
        if not ok or not wait_mount(cfg.mount.drive):
            print("Mount failed. Check WebClient service or port")
            return 1
        print(f"Mounted at {cfg.mount.drive}")
    try:
        while True:
            time.sleep(60)
    except KeyboardInterrupt:
        print("Unmounting...")
        if cfg.mount.drive:
            umount(cfg.mount.drive)
    return 0


def cmd_umount(args: argparse.Namespace) -> int:
    cfg = Config.load()
    if umount(cfg.mount.drive):
        print("Unmounted")
        return 0
    print("Unmount failed or not mounted")
    return 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mcloudmount")
    sub = parser.add_subparsers(dest="command", required=True)

    p_login = sub.add_parser("login", help="Login with phone and SMS code")
    p_login.add_argument("--phone", help="Phone number")
    p_login.set_defaults(func=cmd_login)

    p_logout = sub.add_parser("logout", help="Clear local login state")
    p_logout.set_defaults(func=cmd_logout)

    p_status = sub.add_parser("status", help="Show login and mount status")
    p_status.set_defaults(func=cmd_status)

    p_mount = sub.add_parser("mount", help="Start WebDAV and mount drive")
    p_mount.set_defaults(func=cmd_mount)

    p_umount = sub.add_parser("umount", help="Unmount drive")
    p_umount.set_defaults(func=cmd_umount)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except AuthError as exc:
        print(f"Login state error: {exc}")
        return 2
    except MCloudError as exc:
        print(f"Operation failed: {exc}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
