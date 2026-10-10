// mCloudMount：把移动云盘挂载为 Windows 本地盘符。
//
// 不带参数运行时启动后台服务、托盘图标并打开控制面板；带子命令时作为命令行工具使用。
package main

import (
	"fmt"
	"os"
	"strings"
)

// version 在构建时通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

const usage = `mCloudMount %s —— 把移动云盘挂载为本地盘符

用法:
  mcloudmount                     启动后台服务、托盘图标并打开控制面板
  mcloudmount --background        后台启动（不打开浏览器，开机自启使用）
  mcloudmount <命令> [参数]

命令:
  login [-phone 手机号]           短信验证码登录
  logout                          退出登录并清除本机登录信息
  status [-json]                  显示登录与挂载状态
  mount [-drive Z:] [-webdav-only] 前台挂载，按 Ctrl+C 卸载并退出
  umount                          卸载盘符（后台服务运行时通知它卸载）
  ls [路径]                       列出云盘目录，例如 ls /我的文档
  setup-webclient                 优化 Windows WebClient（需管理员权限）
  version                         显示版本
  help                            显示本帮助

配置与日志目录: %s（可用环境变量 MCLOUDMOUNT_HOME 修改）
`

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if cmd == "" {
		os.Exit(runApp(args))
	}
	// GUI 版本（-H windowsgui）从命令行运行子命令时，连接到父进程的控制台以便输出
	attachConsole()
	var code int
	switch cmd {
	case "login":
		code = cmdLogin(args)
	case "logout":
		code = cmdLogout(args)
	case "status":
		code = cmdStatus(args)
	case "mount":
		code = cmdMount(args)
	case "umount", "unmount":
		code = cmdUmount(args)
	case "ls":
		code = cmdLs(args)
	case "setup-webclient":
		code = cmdSetupWebClient(args)
	case "version", "--version", "-v":
		fmt.Println("mCloudMount", version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		printUsage()
		code = 2
	}
	os.Exit(code)
}
