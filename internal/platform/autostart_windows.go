//go:build windows

package platform

import (
	"errors"
	"os"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey       = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = "mCloudMount"
)

// AutostartSupported 表示是否支持开机自启。
func AutostartSupported() bool { return true }

func launchCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return `"` + exe + `" --background`, nil
}

// AutostartEnabled 表示是否已开启开机自启（且指向当前程序）。
func AutostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValueName)
	return err == nil && v != ""
}

// SetAutostart 开启或关闭开机自启（当前用户，无需管理员权限）。
func SetAutostart(enabled bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !enabled {
		if err := k.DeleteValue(runValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	cmd, err := launchCommand()
	if err != nil {
		return err
	}
	return k.SetStringValue(runValueName, cmd)
}

// RefreshAutostart 程序被移动后更新自启路径。
func RefreshAutostart() {
	if AutostartEnabled() {
		_ = SetAutostart(true)
	}
}
