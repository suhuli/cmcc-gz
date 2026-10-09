"""面板的文件浏览与设置接口测试（使用内存云盘，不访问真实账号）。"""

from __future__ import annotations

import threading
from http.server import ThreadingHTTPServer

import pytest
import requests

from conftest import free_port
from fake_cloud import FakeCloud
from mcloudmount.config import Config
from mcloudmount.panel import Panel, make_handler, validate_name
from mcloudmount.service import MountService
from mcloudmount.errors import MCloudError


@pytest.fixture
def env(tmp_path, monkeypatch):
    monkeypatch.setenv("MCLOUDMOUNT_HOME", str(tmp_path))
    cfg = Config()
    cfg.account.phone = "13800000000"
    cfg.account.token = "t"
    cfg.account.account = "13800000000"
    cfg.save()
    cloud = FakeCloud()
    panel = Panel(MountService(), client_factory=lambda: cloud)
    port = free_port()
    httpd = ThreadingHTTPServer(("127.0.0.1", port), make_handler(panel, {"127.0.0.1", "localhost"}))
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        yield {"url": f"http://127.0.0.1:{port}", "cloud": cloud, "tmp": tmp_path}
    finally:
        httpd.shutdown()
        httpd.server_close()


def post(env, path, body):
    return requests.post(env["url"] + path, json=body, headers={"X-MCM": "1"}, timeout=10)


def test_validate_name_rules():
    assert validate_name("  报告.docx ") == "报告.docx"
    for bad in ["", "   ", ".", "..", "a/b", "a\\b", "x:y", "q?", 'a"b', "<x>", "p|q", "*", "a" * 256]:
        with pytest.raises(MCloudError):
            validate_name(bad)


def test_files_mkdir_list_rename_delete(env):
    r = post(env, "/api/files/mkdir", {"parent": "/", "name": "文档"})
    assert r.status_code == 200, r.text
    folder_id = next(n["fileId"] for n in env["cloud"].nodes.values() if n["name"] == "文档")

    r = post(env, "/api/files/mkdir", {"parent": folder_id, "name": "子目录"})
    assert r.status_code == 200, r.text
    listing = requests.get(env["url"] + "/api/files", params={"folder": folder_id}, timeout=10).json()
    assert [i["name"] for i in listing["items"]] == ["子目录"]
    assert listing["items"][0]["is_dir"] is True

    sub_id = listing["items"][0]["id"]
    r = post(env, "/api/files/rename", {"id": sub_id, "name": "新名字"})
    assert r.status_code == 200, r.text
    root = requests.get(env["url"] + "/api/files", timeout=10).json()
    assert root["items"][0]["is_dir"] is True

    r = post(env, "/api/files/delete", {"ids": [sub_id]})
    assert r.status_code == 200, r.text
    listing = requests.get(env["url"] + "/api/files", params={"folder": folder_id}, timeout=10).json()
    assert listing["items"] == []


def test_files_sorted_folders_first(env):
    cloud = env["cloud"]
    cloud._new_node("/", "b.txt", False)
    cloud._new_node("/", "A", True)
    cloud._new_node("/", "a.txt", False)
    data = requests.get(env["url"] + "/api/files", timeout=10).json()
    assert [i["name"] for i in data["items"]] == ["A", "a.txt", "b.txt"]


def test_invalid_name_returns_400(env):
    r = post(env, "/api/files/mkdir", {"parent": "/", "name": "bad/name"})
    assert r.status_code == 400


def test_delete_requires_selection(env):
    r = post(env, "/api/files/delete", {"ids": []})
    assert r.status_code == 400


def test_files_require_csrf_header(env):
    r = requests.post(env["url"] + "/api/files/mkdir", json={"parent": "/", "name": "x"}, timeout=10)
    assert r.status_code == 403


def test_settings_auto_mount_persists(env):
    r = post(env, "/api/settings", {"auto_mount": True})
    assert r.status_code == 200
    assert r.json()["auto_mount"] is True
    assert Config.load().auto_mount is True
    data = requests.get(env["url"] + "/api/settings", timeout=10).json()
    assert data["auto_mount"] is True
    assert "tray_available" in data and "autostart" in data


def test_open_drive_refused_when_not_mounted(env):
    r = post(env, "/api/open-drive", {})
    assert r.status_code == 400
