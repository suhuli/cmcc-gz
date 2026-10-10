//go:build !windows

package platform

import (
	"context"
	"os"
	"os/exec"
	"runtime"
)

// Supported 表示当前系统支持映射盘符。
func Supported() bool { return false }

func DriveInUse(string) bool                { return false }
func RemoteName(string) string              { return "" }
func Mounted(string, string, int) bool      { return false }
func FreeDrives() []string                  { return nil }
func ForceUnmount(string)                   {}
func AttachParentConsole() bool             { return true }
func GetWebClientInfo() WebClientInfo       { return WebClientInfo{} }
func SetupWebClient() error                 { return ErrUnsupported }
func EnsureWebClient(context.Context) error { return nil }
func RunElevated(...string) (int, error)    { return -1, ErrUnsupported }

func Mount(context.Context, string, string, int, string, string) error { return ErrUnsupported }
func Unmount(context.Context, string) error                            { return nil }

func opener() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}

// OpenPath 用系统文件管理器打开路径。
func OpenPath(p string) error { return exec.Command(opener(), p).Start() }

// OpenURL 用默认浏览器打开网址。
func OpenURL(u string) error { return exec.Command(opener(), u).Start() }

// Alert 在非 Windows 系统上输出到标准错误。
func Alert(title, msg string) { _, _ = os.Stderr.WriteString(title + ": " + msg + "\n") }
