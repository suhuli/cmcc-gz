# mCloudMount

把**中国移动云盘**挂载为 Windows 本地盘符（默认 `Z:`）的小工具。手机号 + 短信验证码登录，挂载后可以在资源管理器里像本地磁盘一样浏览、打开、复制、改名和删除云盘文件。

单个 exe、无需安装任何运行库，自带托盘图标与网页控制面板。

## 功能

- **挂载为盘符**：在本机启动 WebDAV 服务，再通过 Windows 自带的 WebClient 映射为盘符
- **控制面板**（`http://127.0.0.1:8390`）：登录、挂载/卸载、文件管理（浏览、上传、下载、新建文件夹、重命名、删除）、设置与运行日志
- **托盘图标**：左键打开面板，右键挂载/卸载、打开盘符、退出；图标颜色显示状态（绿＝已挂载，橙＝处理中，红＝出错）
- **开机自启**与**启动时自动挂载**（开机时网络未就绪会自动重试）
- **一键优化 WebClient**：把 Windows 默认 50 MB 的单文件上限提高到 4 GB，并设为自动启动（需要一次管理员授权）
- 秒传（内容相同的文件不重复上传）、大文件分片上传、按需读取（Range），中文文件名
- 健康检查：盘符被意外断开时自动重新映射
- 命令行工具：适合脚本与排查问题

## 下载与使用

1. 从 [Releases](../../releases) 下载 `mcloudmount-<版本>-windows-amd64.zip`（ARM 设备选 `arm64`），解压到任意目录。
2. 双击 `mcloudmount.exe`，浏览器会打开控制面板。
3. 输入手机号 → 获取验证码 → 登录。
4. 点击「挂载到 Z:」。挂载成功后在资源管理器中打开 `Z:` 即可。
5. 若概览页提示「建议优化 Windows WebClient 设置」，点击「一键优化」，在弹出的 UAC 窗口中确认。

关闭浏览器页面不会退出程序；要退出请在托盘菜单或面板左下角点击「退出」，盘符会先被卸载。

> 压缩包内另有 `mcloudmount-cli.exe`，是同一程序的控制台版本，便于在命令行中使用。

## 命令行

```
mcloudmount                      启动后台服务、托盘图标并打开控制面板
mcloudmount --background         后台启动（开机自启使用，不打开浏览器）
mcloudmount login [-phone 号码]   短信验证码登录
mcloudmount logout               退出登录并清除本机登录信息
mcloudmount status [-json]       显示登录与挂载状态
mcloudmount mount [-drive Y:] [-webdav-only]
                                 前台挂载，Ctrl+C 卸载并退出
mcloudmount umount               卸载盘符（后台服务运行时会通知它卸载）
mcloudmount ls [路径]            列出云盘目录，例如 ls /我的文档
mcloudmount setup-webclient      优化 WebClient 设置（需管理员权限）
mcloudmount version
```

## 配置与日志

- 目录：`%APPDATA%\mCloudMount\`（可用环境变量 `MCLOUDMOUNT_HOME` 修改）
- `config.json`：登录信息与设置（与旧版 Python 实现兼容，可直接沿用登录态）
- `logs\mcloudmount.log`：运行日志（2 MB 滚动，保留 2 份）

常用设置都可以在面板「设置」页修改：盘符、WebDAV 端口、开机自启、自动挂载。面板端口 `panel_port`（默认 8390）只能在 `config.json` 中修改。

## 常见问题

**挂载失败 / 提示找不到网络路径**
确认 WebClient 服务存在并可启动（`services.msc` → WebClient）。Windows Server 需要先安装「WebDAV 重定向程序」功能。也可以在面板中点击「一键优化」。

**复制大文件时提示文件大小超出限制**
这是 Windows WebClient 的默认 50 MB 限制，点击「一键优化」后，卸载并重新挂载即可。WebClient 的硬上限约为 4 GB；更大的文件请通过面板的上传功能或官方客户端上传。

**盘符被占用**
在「设置 → 挂载 → 盘符」中换一个未使用的盘符。

**资源管理器里看不到刚在别处修改的文件**
目录列表有短暂缓存，按 F5 刷新即可；通过控制面板做的修改会立即生效。

**其他 WebDAV 客户端**
挂载后，本机 WebDAV 服务（默认 `http://127.0.0.1:8380/`）也可以被 RaiDrive、PotPlayer 等客户端使用，用户名和密码见「设置 → WebDAV 连接信息」。服务只监听 127.0.0.1，外部设备无法访问。

## 安全说明

- 所有服务只监听 `127.0.0.1`，并校验 `Host`/`Origin` 头，防止网页通过 DNS 重绑定或跨站请求操作本机服务。
- WebDAV 使用随机生成的密码与 Digest 认证；映射盘符时通过 Windows API 传递密码，不出现在命令行中。
- 登录令牌只保存在本机配置文件中。

## 开发

需要 Go 1.26 或更新版本。

```bash
go test -race ./...                 # 全部测试（使用内存模拟云盘，不访问网络）
GOOS=windows go vet ./...
./build.sh                          # 交叉编译 Windows amd64/arm64 到 dist/
```

Windows 上可运行 `.\build.ps1`。推送 `v*` 标签时，GitHub Actions 会自动构建并发布 Release。

更新 exe 图标或版本信息需要 [go-winres](https://github.com/tc-hib/go-winres)（`go install github.com/tc-hib/go-winres@v0.3.3`），构建脚本检测到后会自动重新生成 `cmd/mcloudmount/rsrc_windows_*.syso`。

### 目录结构

```
cmd/mcloudmount      程序入口：常驻模式（面板 + 托盘）与命令行子命令
internal/cloud       移动云盘接口：登录、签名与传输加密、列表、上传、下载
internal/config      配置文件读写（兼容旧版格式）
internal/vfs         路径 ↔ 文件 ID 映射与目录缓存
internal/davfs       把云盘实现为 WebDAV 文件系统（暂存上传、空文件延迟、一致性等待）
internal/davserver   WebDAV 服务（Digest/Basic 认证、Host 校验、Windows 兼容处理）
internal/service     挂载流程编排与健康检查
internal/panel       控制面板（HTTP API + 内嵌网页）
internal/platform    Windows 相关：映射盘符、WebClient、开机自启、UAC 提权
internal/tray        托盘图标
internal/icon        运行时绘制的程序图标
internal/logx        日志（内存缓冲 + 滚动文件）
```

## 免责声明

本项目与中国移动无关，仅供个人学习与使用。云盘接口未公开，可能随时变化。请遵守云盘服务条款。

## 许可证

[MIT](LICENSE)
