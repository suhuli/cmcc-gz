//go:build !windows

package tray

// Supported 表示当前系统支持托盘图标。
func Supported() bool { return false }

// Running 表示托盘图标是否在运行。
func Running() bool { return false }

// Run 在不支持托盘的系统上直接返回。
func Run(Actions) {}

// Stop 在不支持托盘的系统上不做任何事。
func Stop() {}
