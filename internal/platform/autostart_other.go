//go:build !windows

package platform

// AutostartSupported 表示是否支持开机自启。
func AutostartSupported() bool { return false }

// AutostartEnabled 表示是否已开启开机自启。
func AutostartEnabled() bool { return false }

// SetAutostart 在非 Windows 系统上不受支持。
func SetAutostart(bool) error { return ErrUnsupported }

// RefreshAutostart 在非 Windows 系统上不做任何事。
func RefreshAutostart() {}
