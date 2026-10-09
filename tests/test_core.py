"""纯逻辑单元测试：配置、加解密、签名、状态解析。"""

from __future__ import annotations

import json
import re

import pytest

from mcloudmount import config as config_mod
from mcloudmount.config import Config
from mcloudmount.pc_login import create_signature, decrypt_transport, encrypt_transport


def test_transport_roundtrip():
    text = '{"msisdn":"13800000000","dycpwd":"123456"}'
    assert decrypt_transport(encrypt_transport(text)) == text


def test_signature_is_stable_and_uppercase_hex():
    sig = create_signature('{"a":1}', "20260101 000000", "abcdEFGH12345678")
    assert sig == create_signature('{"a":1}', "20260101 000000", "abcdEFGH12345678")
    assert re.fullmatch(r"[0-9A-F]{32}", sig)


def test_corrupt_config_is_backed_up_not_overwritten(tmp_path, monkeypatch):
    monkeypatch.setenv("MCLOUDMOUNT_HOME", str(tmp_path))
    path = config_mod.config_path()
    path.write_text("{not json", encoding="utf-8")
    cfg = Config.load()
    assert cfg.account.phone == ""
    assert not path.exists(), "损坏文件应被移走"
    backups = list(tmp_path.glob("config.json.bad"))
    assert backups and backups[0].read_text(encoding="utf-8") == "{not json"


def test_valid_config_roundtrip(tmp_path, monkeypatch):
    monkeypatch.setenv("MCLOUDMOUNT_HOME", str(tmp_path))
    cfg = Config.load()
    cfg.account.phone = "13800000000"
    cfg.account.token = "t"
    cfg.save()
    again = Config.load()
    assert again.account.phone == "13800000000"
    assert json.loads(config_mod.config_path().read_text(encoding="utf-8"))["account"]["token"] == "t"


def test_webclient_running_state_regex():
    from mcloudmount.winutil import _RUNNING_STATE

    assert _RUNNING_STATE.search("        STATE              : 4  RUNNING")
    assert _RUNNING_STATE.search("        状态               : 4  正在运行")
    assert not _RUNNING_STATE.search("STATE : 1  STOPPED")


@pytest.mark.parametrize("status,expected", [(404, 404), (403, 403), (503, 500), (500, 500)])
def test_status_mapping(status, expected):
    from mcloudmount.davprovider import _status_error

    assert _status_error(status).value == expected
