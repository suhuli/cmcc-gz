"""网页控制面板的接口测试（不访问真实云盘）。"""

from __future__ import annotations

import threading
from http.server import ThreadingHTTPServer

import pytest
import requests

from conftest import free_port
from mcloudmount.panel import Panel, make_handler
from mcloudmount.service import MountService


@pytest.fixture
def panel_url(tmp_path, monkeypatch):
    monkeypatch.setenv("MCLOUDMOUNT_HOME", str(tmp_path))
    panel = Panel(MountService())
    port = free_port()
    httpd = ThreadingHTTPServer(("127.0.0.1", port), make_handler(panel, {"127.0.0.1", "localhost"}))
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{port}"
    finally:
        httpd.shutdown()
        httpd.server_close()


def test_index_served(panel_url):
    resp = requests.get(panel_url + "/", timeout=5)
    assert resp.status_code == 200
    assert "mCloudMount" in resp.text
    assert "text/html" in resp.headers["Content-Type"]


def test_status_shape_when_logged_out(panel_url):
    data = requests.get(panel_url + "/api/status", timeout=5).json()
    assert data["logged_in"] is False
    assert data["phase"] == "idle"
    assert data["mounted"] is False
    assert {"drive", "webdav", "server_running", "message", "error"} <= set(data)


def test_post_requires_csrf_header(panel_url):
    resp = requests.post(panel_url + "/api/mount", json={}, timeout=5)
    assert resp.status_code == 403


def test_foreign_host_rejected(panel_url):
    resp = requests.get(panel_url + "/api/status", headers={"Host": "evil.example"}, timeout=5)
    assert resp.status_code == 403


def test_invalid_phone_rejected_before_network(panel_url):
    resp = requests.post(
        panel_url + "/api/login/send",
        json={"phone": "123"},
        headers={"X-MCM": "1"},
        timeout=5,
    )
    assert resp.status_code == 400
    assert "11 位" in resp.json()["error"]


def test_mount_refused_when_not_logged_in(panel_url):
    resp = requests.post(panel_url + "/api/mount", json={}, headers={"X-MCM": "1"}, timeout=5)
    # 挂载在后台线程执行，接口本身返回已接受；状态中应体现错误
    assert resp.status_code == 200
    import time

    deadline = time.time() + 5
    while time.time() < deadline:
        status = requests.get(panel_url + "/api/status", timeout=5).json()
        if status["phase"] == "error":
            break
        time.sleep(0.1)
    assert status["phase"] == "error"
    assert "请先登录" in status["error"]


def test_static_path_traversal_blocked(panel_url):
    resp = requests.get(panel_url + "/static/../../pyproject.toml", timeout=5)
    assert resp.status_code in (403, 404)
    assert "[project]" not in resp.text


def test_logs_endpoint(panel_url):
    data = requests.get(panel_url + "/api/logs", timeout=5).json()
    assert isinstance(data["lines"], list)
