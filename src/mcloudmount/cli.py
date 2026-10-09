"""命令行入口。"""

from __future__ import annotations

import argparse
import atexit
import json
import logging
import signal
import threading
import time

from .api import MCloudClient
from .config import Config
from .errors import AuthError, MCloudError
from .service import MountError, MountService
from .winutil import mount_status, umount

log = logging.getLogger("mcloudmount")


def _configure_logging(level: str) -> None:
    logging.basicConfig(
        level=getattr(logging, level.upper(), logging.INFO),
        format="%(asctime)s %(levelname)s %(message)s",
    )


def _make_client(cfg: Config) -> MCloudClient:
    return MCloudClient(cfg)


def _token_expired(cfg: Config) -> bool:
    expire = cfg.account.token_expire_ms
    return bool(expire and expire <= time.time() * 1000)


def cmd_login(args: argparse.Namespace) -> int:
    cfg = Config.load()
    client = _make_client(cfg)
    phone = args.phone or input("手机号: ").strip()
    if not phone:
        print("手机号不能为空")
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
        # 不输出服务端原始内容，避免把令牌等字段打印到终端
        code_text = login_result.get("return") or login_result.get("code") or "未知"
        print(f"登录失败（返回码 {code_text}），请确认验证码是否正确或稍后重试")
        return 2
    client.save_account(phone, login_result)
    print("登录成功，正在协商文件协议...")
    data = client.resolve_connection()
    profile = data.get("profile") or client.cfg.account.ext_info.get("profile")
    print(f"文件协议协商完成: {profile}")
    print(f"个人网盘地址: {client.personal_url}")
    return 0


def cmd_logout(args: argparse.Namespace) -> int:
    cfg = Config.load()
    if cfg.mount.drive and mount_status(cfg.mount.drive):
        umount(cfg.mount.drive)
    cfg.account = type(cfg.account)()
    cfg.save()
    print("已清除本地登录状态")
    return 0


def cmd_status(args: argparse.Namespace) -> int:
    cfg = Config.load()
    account = cfg.account
    info = {
        "logged_in": bool(account.phone and account.token),
        "account": account.phone,
        "token_expired": _token_expired(cfg),
        "drive": cfg.mount.drive,
        "mounted": bool(cfg.mount.drive and mount_status(cfg.mount.drive)),
        "webdav": f"http://{cfg.mount.host}:{cfg.mount.port}",
    }
    if args.json:
        print(json.dumps(info, ensure_ascii=False, indent=2))
        return 0 if info["logged_in"] else 1
    if not info["logged_in"]:
        print("未登录")
        return 1
    print(f"账号: {info['account']}")
    print(f"令牌: {'已过期（需重新登录）' if info['token_expired'] else '有效'}")
    print(f"盘符: {info['drive']} ({'已挂载' if info['mounted'] else '未挂载'})")
    print(f"WebDAV: {info['webdav']}")
    return 0


def cmd_mount(args: argparse.Namespace) -> int:
    cfg = Config.load()
    _configure_logging(cfg.log_level)
    svc = MountService(cfg)
    try:
        svc.start()
    except MountError as exc:
        print(exc.message)
        return 1
    print(svc.state.message)

    stop = threading.Event()

    def _on_signal(signum, frame) -> None:  # noqa: ARG001
        stop.set()

    atexit.register(svc.stop)
    for name in ("SIGINT", "SIGTERM", "SIGBREAK"):
        sig = getattr(signal, name, None)
        if sig is not None:
            try:
                signal.signal(sig, _on_signal)
            except (ValueError, OSError):
                pass

    try:
        while not stop.wait(5):
            svc.check_health()
            if svc.state.phase == "error":
                log.error(svc.state.error)
                return 1
    finally:
        print("正在卸载...")
        svc.stop()
    return 0


def cmd_panel(args: argparse.Namespace) -> int:
    from .panel import DEFAULT_PANEL_PORT, serve

    return serve(port=args.port or DEFAULT_PANEL_PORT, open_browser=not args.no_browser)


def cmd_umount(args: argparse.Namespace) -> int:
    cfg = Config.load()
    if umount(cfg.mount.drive):
        print("已卸载")
        return 0
    print("卸载失败，或盘符未挂载")
    return 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mcloudmount")
    parser.set_defaults(func=cmd_panel, port=None, no_browser=False)
    sub = parser.add_subparsers(dest="command")

    p_login = sub.add_parser("login", help="手机号 + 短信验证码登录")
    p_login.add_argument("--phone", help="手机号")
    p_login.set_defaults(func=cmd_login)

    p_logout = sub.add_parser("logout", help="清除本地登录状态")
    p_logout.set_defaults(func=cmd_logout)

    p_status = sub.add_parser("status", help="查看登录与挂载状态")
    p_status.add_argument("--json", action="store_true", help="以 JSON 输出（供界面调用）")
    p_status.set_defaults(func=cmd_status)

    p_mount = sub.add_parser("mount", help="启动 WebDAV 并挂载盘符")
    p_mount.set_defaults(func=cmd_mount)

    p_umount = sub.add_parser("umount", help="卸载盘符")
    p_umount.set_defaults(func=cmd_umount)

    p_panel = sub.add_parser("panel", help="打开网页控制面板（默认）")
    p_panel.add_argument("--port", type=int, default=None, help="面板端口，默认 8390")
    p_panel.add_argument("--no-browser", action="store_true", help="不自动打开浏览器")
    p_panel.set_defaults(func=cmd_panel)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except AuthError as exc:
        print(f"登录状态异常: {exc.message}")
        return 2
    except MCloudError as exc:
        print(f"操作失败: {exc.message}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
