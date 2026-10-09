# mCloudMount

把中国移动云盘挂载为 Windows 本地磁盘的工具。支持手机号 + 短信验证码登录，当前通过本地 WebDAV 服务映射为盘符（默认 `Z:`）。

## 功能

- 手机号 + 短信验证码登录
- 登录态保存在本机，避免反复接收验证码
- 将云盘挂载为 Windows 磁盘
- 目录浏览、文件上传、下载、删除、重命名、移动、复制
- 中文文件名、空文件、分片上传、断点上传、Range 读取
- 适合日常文件访问，当前后端为 WebDAV，后续可扩展 WinFsp

## 环境要求

- Windows 10/11
- Python 3.10+
- WebClient 服务可用（Windows 系统自带）
- 首次挂载建议以管理员权限运行，便于启动 WebClient 服务和调整相关限制

## 安装

```powershell
python -m venv .venv
.\.venv\Scripts\Activate.ps1
pip install -e .
```

## 使用

```powershell
python -m mcloudmount login --phone 13800000000
python -m mcloudmount mount
python -m mcloudmount status
python -m mcloudmount umount
```

登录时会在终端等待短信验证码。验证码成功登录后，授权信息会保存到：

```text
%APPDATA%\mCloudMount\config.json
```

不要把这个文件提交到仓库或分享给他人。

## 构建 EXE

```powershell
pip install pyinstaller
.\build.ps1
```

构建产物位于：

```text
dist\mcloudmount.exe
```

也可以直接运行：

```powershell
dist\mcloudmount.exe login --phone 13800000000
dist\mcloudmount.exe mount
```

## 测试

单元与端到端测试（内存云盘，不需要账号，可在任意系统运行）：

```powershell
pip install -e ".[dev]"
pytest
```

离线 DAV 冒烟测试：

```powershell
python scripts\local_dav_smoke.py
```

真实云盘回归测试会读取本机已保存的登录态，并在云盘根目录创建独立的 `__mcm_regression_*` 目录，测试完成后自动清理：

```powershell
python scripts\deep_dav_regression.py
```

## 安全说明

- 本工具只保存本机使用所需的登录态和配置。
- 不会上传你的本地配置到任何第三方服务。
- 如果不再使用，可删除 `%APPDATA%\mCloudMount\config.json`。
- 中国移动云盘接口可能会调整，若服务端变更，项目需要同步适配。

## License

MIT
