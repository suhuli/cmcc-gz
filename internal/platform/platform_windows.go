//go:build windows

package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// Supported 表示当前系统支持映射盘符。
func Supported() bool { return true }

var (
	mpr                       = windows.NewLazySystemDLL("mpr.dll")
	procWNetAddConnection2W   = mpr.NewProc("WNetAddConnection2W")
	procWNetCancelConnection2 = mpr.NewProc("WNetCancelConnection2W")
	procWNetGetConnectionW    = mpr.NewProc("WNetGetConnectionW")

	shell32             = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteExW = shell32.NewProc("ShellExecuteExW")

	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
)

const (
	resourcetypeDisk     = 1
	errorNotConnected    = 2250
	errorMoreData        = 234
	errorAlreadyAssigned = 85
	errorBadNetName      = 67
	errorSessionCred     = 1219
	errorBadDevice       = 1200
	errorDeviceInUse     = 2404
	errorOpenFiles       = 2401
	createNoWindow       = 0x08000000
	webClientParamsKey   = `SYSTEM\CurrentControlSet\Services\WebClient\Parameters`
)

type netResource struct {
	Scope       uint32
	Type        uint32
	DisplayType uint32
	Usage       uint32
	LocalName   *uint16
	RemoteName  *uint16
	Comment     *uint16
	Provider    *uint16
}

func u16(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// withTimeout 在后台执行可能长时间阻塞的系统调用。
func withTimeout(ctx context.Context, d time.Duration, fn func() error) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return fmt.Errorf("操作超时（%s），Windows WebClient 服务可能没有响应", d)
	}
}

func driveBit(drive string) uint32 {
	if len(drive) < 1 {
		return 0
	}
	c := strings.ToUpper(drive)[0]
	if c < 'A' || c > 'Z' {
		return 0
	}
	return 1 << (c - 'A')
}

// DriveInUse 表示盘符当前是否被占用（本地磁盘或网络映射）。
func DriveInUse(drive string) bool {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return false
	}
	return mask&driveBit(drive) != 0
}

// RemoteName 返回盘符映射的远程路径；未映射时返回空。
func RemoteName(drive string) string {
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	r, _, _ := procWNetGetConnectionW.Call(uintptr(unsafe.Pointer(u16(drive))), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// Mounted 表示盘符是否映射到了 host:port 上的本程序服务。
func Mounted(drive, host string, port int) bool {
	if drive == "" || !DriveInUse(drive) {
		return false
	}
	return IsOurMapping(RemoteName(drive), host, port)
}

// FreeDrives 返回可用的盘符（D: 到 Z:，倒序，常用的 Z: 在前）。
func FreeDrives() []string {
	mask, _ := windows.GetLogicalDrives()
	var out []string
	for c := 'Z'; c >= 'D'; c-- {
		if mask&(1<<(c-'A')) == 0 {
			out = append(out, string(c)+":")
		}
	}
	return out
}

func wnetError(code uintptr) error {
	switch code {
	case errorAlreadyAssigned:
		return errors.New("盘符已被占用，请在设置中换一个盘符")
	case errorBadNetName:
		return errors.New("找不到 WebDAV 服务：请确认 Windows WebClient 服务可用")
	case errorSessionCred:
		return errors.New("已存在使用其他凭据的连接，请先断开相关的网络驱动器")
	case errorBadDevice:
		return errors.New("盘符无效")
	}
	return fmt.Errorf("映射盘符失败（错误码 %d）: %v", code, syscall.Errno(code))
}

// Mount 把 WebDAV 根目录映射为盘符。
func Mount(ctx context.Context, drive, host string, port int, user, password string) error {
	unc := UNC(host, port)
	err := withTimeout(ctx, 90*time.Second, func() error {
		nr := netResource{Type: resourcetypeDisk, LocalName: u16(drive), RemoteName: u16(unc)}
		r, _, _ := procWNetAddConnection2W.Call(uintptr(unsafe.Pointer(&nr)), uintptr(unsafe.Pointer(u16(password))), uintptr(unsafe.Pointer(u16(user))), 0)
		if r != 0 {
			return wnetError(r)
		}
		return nil
	})
	if err == nil {
		return nil
	}
	// 接口调用失败时退回到 net use（个别系统上行为更稳定）
	slog.Warn("WNetAddConnection2 失败，改用 net use", "error", err)
	out, cerr := run(ctx, 90*time.Second, "net", "use", drive, unc, password, "/user:"+user, "/persistent:no")
	if cerr != nil {
		return fmt.Errorf("%v；net use: %s", err, firstLine(out))
	}
	return nil
}

// Unmount 断开盘符。盘符已不存在时返回 nil。
func Unmount(ctx context.Context, drive string) error {
	if drive == "" || !DriveInUse(drive) {
		return nil
	}
	cancel := func(force uintptr) uintptr {
		var r uintptr
		_ = withTimeout(ctx, 20*time.Second, func() error {
			r, _, _ = procWNetCancelConnection2.Call(uintptr(unsafe.Pointer(u16(drive))), 0, force)
			return nil
		})
		return r
	}
	r := cancel(0)
	if r == 0 || r == errorNotConnected {
		return nil
	}
	if r == errorOpenFiles || r == errorDeviceInUse {
		time.Sleep(time.Second)
		if r = cancel(0); r == 0 || r == errorNotConnected {
			return nil
		}
		return fmt.Errorf("盘符 %s 正在被使用，关闭相关窗口或程序后重试", drive)
	}
	out, err := run(ctx, 20*time.Second, "net", "use", drive, "/delete", "/yes")
	if err != nil && DriveInUse(drive) {
		return fmt.Errorf("断开盘符失败: %s", firstLine(out))
	}
	return nil
}

// ForceUnmount 强制断开（退出程序时使用，打开的文件会被关闭）。
func ForceUnmount(drive string) {
	if drive == "" {
		return
	}
	procWNetCancelConnection2.Call(uintptr(unsafe.Pointer(u16(drive))), 0, 1)
}

// ---------------------------------------------------------------- WebClient

func openWebClient(access uint32) (windows.Handle, windows.Handle, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, 0, err
	}
	svc, err := windows.OpenService(scm, u16("WebClient"), access)
	if err != nil {
		windows.CloseServiceHandle(scm)
		return 0, 0, err
	}
	return scm, svc, nil
}

func webClientRunning() (running bool, auto bool, ok bool) {
	scm, svc, err := openWebClient(windows.SERVICE_QUERY_STATUS | windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return false, false, false
	}
	defer windows.CloseServiceHandle(scm)
	defer windows.CloseServiceHandle(svc)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(svc, &st); err != nil {
		return false, false, false
	}
	var need uint32
	_ = windows.QueryServiceConfig(svc, nil, 0, &need)
	if need > 0 {
		buf := make([]byte, need)
		cfg := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buf[0]))
		if windows.QueryServiceConfig(svc, cfg, need, &need) == nil {
			auto = cfg.StartType == windows.SERVICE_AUTO_START
		}
	}
	return st.CurrentState == windows.SERVICE_RUNNING, auto, true
}

// EnsureWebClient 确保 WebClient 服务在运行。没有权限启动时返回错误（映射盘符时系统通常会自动触发启动）。
func EnsureWebClient(ctx context.Context) error {
	if running, _, ok := webClientRunning(); ok && running {
		return nil
	}
	scm, svc, err := openWebClient(windows.SERVICE_START | windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return fmt.Errorf("无法访问 WebClient 服务: %w", err)
	}
	defer windows.CloseServiceHandle(scm)
	defer windows.CloseServiceHandle(svc)
	if err := windows.StartService(svc, 0, nil); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("启动 WebClient 服务失败: %w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var st windows.SERVICE_STATUS
		if windows.QueryServiceStatus(svc, &st) == nil && st.CurrentState == windows.SERVICE_RUNNING {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return errors.New("WebClient 服务启动超时")
}

// GetWebClientInfo 读取 WebClient 服务状态与单文件大小上限。
func GetWebClientInfo() WebClientInfo {
	info := WebClientInfo{Supported: true}
	info.Running, info.AutoStart, _ = webClientRunning()
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, webClientParamsKey, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetIntegerValue("FileSizeLimitInBytes"); err == nil {
			info.FileSizeLimit = uint32(v)
		}
		if v, _, err := k.GetIntegerValue("SendReceiveTimeoutInSec"); err == nil {
			info.Timeout = uint32(v)
		}
		k.Close()
	}
	if info.FileSizeLimit == 0 {
		info.FileSizeLimit = DefaultFileSizeLimit
	}
	if info.Timeout == 0 {
		info.Timeout = DefaultTimeout
	}
	info.NeedsFix = info.FileSizeLimit < RecommendedFileSizeLimit || info.Timeout < RecommendedTimeout || !info.AutoStart
	return info
}

// SetupWebClient 需要管理员权限：放宽单文件大小上限与请求超时、设为自动启动并重启服务。
func SetupWebClient() error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, webClientParamsKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("写入注册表失败（需要管理员权限）: %w", err)
	}
	err = k.SetDWordValue("FileSizeLimitInBytes", RecommendedFileSizeLimit)
	if err == nil {
		err = k.SetDWordValue("SendReceiveTimeoutInSec", RecommendedTimeout)
	}
	k.Close()
	if err != nil {
		return fmt.Errorf("写入注册表失败: %w", err)
	}
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(scm)
	svc, err := windows.OpenService(scm, u16("WebClient"), windows.SERVICE_CHANGE_CONFIG|windows.SERVICE_START|windows.SERVICE_STOP|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return fmt.Errorf("打开 WebClient 服务失败: %w", err)
	}
	defer windows.CloseServiceHandle(svc)
	if err := windows.ChangeServiceConfig(svc, windows.SERVICE_NO_CHANGE, windows.SERVICE_AUTO_START, windows.SERVICE_NO_CHANGE, nil, nil, nil, nil, nil, nil, nil); err != nil {
		return fmt.Errorf("设置自动启动失败: %w", err)
	}
	// 重启服务使新的上限生效
	var st windows.SERVICE_STATUS
	_ = windows.ControlService(svc, windows.SERVICE_CONTROL_STOP, &st)
	for i := 0; i < 50; i++ {
		if windows.QueryServiceStatus(svc, &st) == nil && st.CurrentState == windows.SERVICE_STOPPED {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := windows.StartService(svc, 0, nil); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("重启 WebClient 服务失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- 进程与外壳

type shellExecuteInfo struct {
	cbSize         uint32
	fMask          uint32
	hwnd           uintptr
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       uintptr
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      uintptr
	dwHotKey       uint32
	hIconOrMonitor uintptr
	hProcess       windows.Handle
}

const seeMaskNoCloseProcess = 0x00000040

// RunElevated 以管理员身份运行本程序的子命令（弹出 UAC 确认），等待其结束并返回退出码。
func RunElevated(args ...string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return -1, err
	}
	params := make([]string, len(args))
	for i, a := range args {
		params[i] = syscall.EscapeArg(a)
	}
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess,
		lpVerb:       u16("runas"),
		lpFile:       u16(exe),
		lpParameters: u16(strings.Join(params, " ")),
		nShow:        windows.SW_HIDE,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	r, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		if errors.Is(callErr, windows.ERROR_CANCELLED) {
			return -1, errors.New("已取消管理员授权")
		}
		return -1, fmt.Errorf("无法以管理员身份运行: %v", callErr)
	}
	if info.hProcess == 0 {
		return 0, nil
	}
	defer windows.CloseHandle(info.hProcess)
	if _, err := windows.WaitForSingleObject(info.hProcess, 2*60*1000); err != nil {
		return -1, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.hProcess, &code); err != nil {
		return -1, err
	}
	return int(code), nil
}

// OpenPath 在资源管理器中打开文件夹或盘符。
func OpenPath(p string) error {
	return windows.ShellExecute(0, u16("open"), u16(p), nil, nil, windows.SW_SHOWNORMAL)
}

// OpenURL 用默认浏览器打开网址。
func OpenURL(u string) error {
	return windows.ShellExecute(0, u16("open"), u16(u), nil, nil, windows.SW_SHOWNORMAL)
}

// AttachParentConsole 让 GUI 程序在命令行中运行时能够输出文字。
func AttachParentConsole() bool {
	r, _, _ := procAttachConsole.Call(uintptr(^uint32(0)))
	if r == 0 {
		return false
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
		os.Stdin = f
	}
	return true
}

// run 执行系统命令（不显示窗口），输出按系统代码页解码。
func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	out := buf.Bytes()
	if !utf8.Valid(out) {
		if dec, derr := simplifiedchinese.GBK.NewDecoder().Bytes(out); derr == nil {
			out = dec
		}
	}
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("命令超时: %s", name)
	}
	return string(out), err
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}

var procMessageBoxW = windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW")

// Alert 弹出消息框（GUI 程序没有控制台时用于显示致命错误）。
func Alert(title, msg string) {
	const mbIconError, mbSetForeground = 0x10, 0x10000
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16(msg))), uintptr(unsafe.Pointer(u16(title))), mbIconError|mbSetForeground)
}
