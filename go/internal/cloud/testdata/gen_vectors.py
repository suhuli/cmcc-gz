"""生成 Go 版的跨语言测试向量（由 Python 参考实现产出）。

用法：PYTHONPATH=src python3 go/internal/cloud/testdata/gen_vectors.py
输出：go/internal/cloud/testdata/vectors.json
"""
import base64
import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(ROOT / "src"))

from Crypto.Cipher import AES  # noqa: E402
from Crypto.Util.Padding import pad  # noqa: E402

from mcloudmount import api, pc_login  # noqa: E402

bodies = [
    '{"parentFileId":"/","parentFilePath":false,"pageInfo":{"pageSize":200,"pageCursor":"","needTotalCount":0}}',
    '{"name":"报告 final (v2).docx","x":"a!b\'c(d)e*f~g_h.i-j"}',
    '{"fileId":"abc123","name":"<tag>&\\"quote\\"\\\\"}',
    '{"k":"中文路径/子目录/文件 名称.txt"}',
    '{}',
]
sign_cases = []
for i, body in enumerate(bodies):
    ts = f"20261009 12{i:02d}00"
    nonce = f"NONCE{i:010d}"[:16]
    sign_cases.append({
        "body": body,
        "timestamp": ts,
        "nonce": nonce,
        "encoded": pc_login.encode_uri_component(body),
        "signature": pc_login.create_signature(body, ts, nonce),
    })

iv = bytes(range(16))
transport_cases = []
for body in bodies[:3]:
    wire = pc_login.encrypt_transport(body, iv=iv)
    transport_cases.append({"clear": body, "iv_hex": iv.hex(), "wire": wire})

# 登录数据（ECB，hex）：Go 端解密应还原明文
login_plain = '{"authToken":"tok123","account":"17671400063","return":"0"}'
ecb = AES.new(pc_login.LOGIN_DATA_KEY, AES.MODE_ECB)
login_hex = ecb.encrypt(pad(login_plain.encode("utf-8"), AES.block_size)).hex()

out = {
    "sign": sign_cases,
    "transport": transport_cases,
    "login_data": {"plain": login_plain, "hex": login_hex},
    "basic_auth": {"kind": "pc", "account": "17671400063", "token": "tok123",
                   "expect": pc_login.basic_auth("pc", "17671400063", "tok123")},
    "device_info": {"expect": api._web_device_info("web-0063")},
    "client_info": {"expect": api._web_client_info("web-0063")},
}
dest = Path(__file__).with_name("vectors.json")
dest.write_text(json.dumps(out, ensure_ascii=False, indent=2), encoding="utf-8")
print("wrote", dest, "sign:", len(sign_cases), "transport:", len(transport_cases))
