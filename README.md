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

### 网页控制面板（推荐）

双击 `mcloudmount.exe`（或执行 `mcloudmount` / `mcloudmount panel`），会自动在浏览器打开控制面板：

- 手机号 + 短信验证码登录
- 一键挂载 / 卸载盘符
- 查看登录状态、盘符、WebDAV 服务状态
- 查看运行日志

面板只监听 `127.0.0.1:8390`，关闭控制台窗口即退出，退出前会自动卸载盘符。

### 命令行

```powershell
python -m mcloudmount login --phone 13800000000
python -m mcloudmount mount
python -m mcloudmount status
python -m mcloudmount status --json
python -m mcloudmount umount
```

登录后授权信息保存在 `%APPDATA%\mCloudMount\config.json`。若该文件损坏，程序会将其备份为 `config.json.bad` 并要求重新登录。

## 构建 EXE

在 PowerShell 中于项目根目录执行：

```powershell
pip install pyinstaller
.\build.ps1
```

产物位于 `dist\mcloudmount.exe`（已包含网页面板资源）。

也可以不在本机构建：推送到 GitHub 后，Actions 中的 `build` 工作流会在 Windows 环境运行测试并构建，
在运行页面下载 `mcloudmount-exe` 即可。

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
