# mCloudMount（Go 重写，M0）

本目录是重写版的起点，与 Python 版并存，直到新版达到对等功能。

## 当前状态（M0：协议移植与只读烟测）

| 模块 | 状态 | 验证方式 |
|---|---|---|
| 签名 `mcloud-sign` | 完成 | 与 Python 逐字节一致（`testdata/vectors.json`） |
| 传输加解密（AES-CBC、登录数据 ECB） | 完成 | 与 Python 逐字节一致 |
| 个人云：列目录（自动翻页） | 完成 | 真实账号：7 项，与 Python 版一致 |
| 个人云：下载地址、Range 读取 | 完成 | 真实账号：整文件与 Range 片段 SHA256 与 Python 一致 |
| 连接协商 | 完成，改为先试已保存组合、成功即停 | 真实账号：1 次请求即成功 |
| 登录（短信发送、验证码登录） | 已移植，未做真实登录测试 | 本轮没有新的验证码，留待需要时验证 |
| 写入类接口（建目录、改名、移动、删除、上传） | 未开始 | M1–M2 |

## 与 Python 版相比修复的问题
- `ensureOK`：空状态码不再被当成成功，缺失状态码视为错误。真实账号上的正常响应均通过，没有回归。
- 连接协商：不再固定尝试最多 12 种组合。

## 运行

```
cd go
go test ./...
go build -o probe.exe ./cmd/probe
probe -resolve -list /                     # 只读：协商连接、列根目录
probe -download <fileId> -limit 0          # 只读：整文件读取并输出 SHA256
probe -download <fileId> -offset 1000 -limit 5000
```

配置文件默认读取 `MCLOUDMOUNT_HOME/config.json`（与 Python 版相同格式），输出不包含令牌。

## 重新生成跨语言向量
```
PYTHONPATH=src python3 go/internal/cloud/testdata/gen_vectors.py
```
