// Package tray 提供 Windows 托盘图标：左键打开控制面板，右键菜单挂载/卸载、打开盘符、退出。
package tray

import "mcloudmount/internal/service"

// Actions 是托盘菜单触发的操作。
type Actions struct {
	OpenPanel func()
	Mount     func()
	Unmount   func()
	OpenDrive func()
	Quit      func()
	// Status 返回当前状态（用于刷新图标与菜单）。
	Status func() service.Status
	// LoggedIn 返回是否已登录。
	LoggedIn func() bool
}
