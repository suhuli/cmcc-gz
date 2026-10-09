"""WebDAV 适配层的端到端测试（内存云盘 + 真实 WsgiDAV/cheroot）。"""

from __future__ import annotations

import random



def put(dav, path: str, data: bytes, expect=(200, 201, 204)):
    resp = dav.http.put(dav.url(path), data=data, timeout=120)
    assert resp.status_code in expect, f"PUT {path} -> {resp.status_code} {resp.text[:200]}"
    return resp


def get(dav, path: str, headers=None):
    return dav.http.get(dav.url(path), headers=headers or {}, timeout=120)


def test_put_get_small_and_multipart(dav):
    dav.http.request("MKCOL", dav.url("/d"))
    small = b"hello mcloud"
    put(dav, "/d/small.txt", small)
    assert get(dav, "/d/small.txt").content == small

    big = random.Random(1).randbytes(9 * 1024 * 1024 + 123)  # 跨越 2 个 8MB 分片
    put(dav, "/d/big.bin", big)
    assert dav.cloud_.content_of(_id(dav, "/d"), "big.bin") == big
    assert get(dav, "/d/big.bin").content == big
    assert dav.cloud_.calls["upload_part"] == 2 + 1  # small(1) + big(2)


def test_overwrite_replaces_content_without_residue(dav):
    dav.http.request("MKCOL", dav.url("/o"))
    put(dav, "/o/f.bin", b"first")
    put(dav, "/o/f.bin", b"second version")
    assert get(dav, "/o/f.bin").content == b"second version"
    names = dav.cloud_.names_in(_id(dav, "/o"))
    assert names == ["f.bin"], names


def test_failed_overwrite_restores_original(dav):
    dav.http.request("MKCOL", dav.url("/r"))
    put(dav, "/r/keep.bin", b"original")
    dav.cloud_.fail_new_rename = True
    resp = dav.http.put(dav.url("/r/keep.bin"), data=b"new content", timeout=120)
    dav.cloud_.fail_new_rename = False
    assert resp.status_code >= 500
    assert get(dav, "/r/keep.bin").content == b"original"
    assert dav.cloud_.names_in(_id(dav, "/r")) == ["keep.bin"], "失败后不应留下临时/备份文件"


def test_move_file_is_server_side(dav):
    dav.http.request("MKCOL", dav.url("/m"))
    dav.http.request("MKCOL", dav.url("/m/sub"))
    payload = random.Random(2).randbytes(256 * 1024)
    put(dav, "/m/a.bin", payload)
    uploads_before = dav.cloud_.calls.get("upload_create", 0)
    downloads_before = dav.cloud_.calls.get("download", 0)
    resp = dav.http.request(
        "MOVE", dav.url("/m/a.bin"),
        headers={"Destination": dav.url("/m/sub/b.bin"), "Overwrite": "F"}, timeout=120,
    )
    assert resp.status_code in (201, 204), resp.text[:200]
    assert dav.cloud_.calls.get("upload_create", 0) == uploads_before, "MOVE 不应重新上传"
    assert dav.cloud_.calls.get("download", 0) == downloads_before, "MOVE 不应重新下载"
    assert get(dav, "/m/sub/b.bin").content == payload
    assert get(dav, "/m/a.bin").status_code == 404


def test_range_read_when_server_ignores_range(dav):
    dav.http.request("MKCOL", dav.url("/g"))
    data = bytes(range(256)) * 64
    put(dav, "/g/range.bin", data)
    dav.cloud_.ignore_range = True
    try:
        resp = get(dav, "/g/range.bin", headers={"Range": "bytes=100-199"})
    finally:
        dav.cloud_.ignore_range = False
    assert resp.status_code == 206
    assert resp.content == data[100:200]


def test_download_server_error_is_not_reported_as_404(dav):
    dav.http.request("MKCOL", dav.url("/e"))
    put(dav, "/e/x.bin", b"abc")
    dav.cloud_.download_status = 503
    try:
        resp = get(dav, "/e/x.bin")
    finally:
        dav.cloud_.download_status = None
    assert resp.status_code == 500


def test_case_insensitive_lookup(dav):
    dav.http.request("MKCOL", dav.url("/c"))
    put(dav, "/c/README.TXT", b"upper")
    assert get(dav, "/c/readme.txt").content == b"upper"


def test_delete_goes_to_trash(dav):
    dav.http.request("MKCOL", dav.url("/t"))
    put(dav, "/t/gone.bin", b"x")
    resp = dav.http.delete(dav.url("/t/gone.bin"), timeout=60)
    assert resp.status_code in (200, 204)
    assert dav.cloud_.calls.get("trash", 0) >= 1
    assert get(dav, "/t/gone.bin").status_code == 404


def test_server_stop_releases_port(cloud):
    import socket

    from mcloudmount.davserver import DavServer
    from conftest import free_port

    port = free_port()
    server = DavServer(cloud, "127.0.0.1", port, "u", "p")
    assert server.start_background(wait=10)
    assert server.is_running()
    server.stop()
    assert not server.is_running()
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", port))  # 端口应已释放


def _id(dav, path: str) -> str:
    entry = dav.cloud_.nodes
    parts = [p for p in path.strip("/").split("/") if p]
    parent = "/"
    for part in parts:
        node = next(n for n in entry.values() if n["parent"] == parent and n["name"] == part and not n["trashed"])
        parent = node["fileId"]
    return parent
