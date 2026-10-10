//go:build windows

package tray

import (
	"runtime"
	"sync/atomic"
	"time"

	"fyne.io/systray"

	"mcloudmount/internal/icon"
	"mcloudmount/internal/service"
)

var running atomic.Bool

// Supported 表示当前系统支持托盘图标。
func Supported() bool { return true }

// Running 表示托盘图标是否在运行。
func Running() bool { return running.Load() }

// Run 显示托盘图标并阻塞，直到 Stop 被调用。必须在主 goroutine 中调用。
func Run(a Actions) {
	runtime.LockOSThread()
	icons := map[icon.State][]byte{}
	for _, s := range []icon.State{icon.Idle, icon.Running, icon.Busy, icon.Error} {
		icons[s] = icon.ICO(s, 16, 20, 24, 32, 40, 48)
	}
	stop := make(chan struct{})
	systray.Run(func() {
		running.Store(true)
		systray.SetIcon(icons[icon.Idle])
		systray.SetTitle("mCloudMount")
		systray.SetTooltip("mCloudMount 云盘挂载")
		systray.SetOnTapped(a.OpenPanel)

		mStatus := systray.AddMenuItem("未挂载", "")
		mStatus.Disable()
		systray.AddSeparator()
		mPanel := systray.AddMenuItem("打开控制面板", "")
		mToggle := systray.AddMenuItem("挂载", "")
		mDrive := systray.AddMenuItem("打开盘符", "")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "卸载盘符并退出程序")

		var last string
		refresh := func() {
			st := a.Status()
			logged := a.LoggedIn()
			state, text, tip := icon.Idle, "未挂载", "mCloudMount：未挂载"
			switch {
			case st.Phase == service.PhaseStarting || st.Phase == service.PhaseStopping:
				state, text = icon.Busy, st.Message
			case st.Phase == service.PhaseError:
				state, text = icon.Error, "出错："+st.Error
			case st.Phase == service.PhaseRunning:
				state, text = icon.Running, "已挂载到 "+st.Drive
			case !logged:
				text = "未登录"
			}
			if text == "" {
				text = "处理中…"
			}
			if len([]rune(text)) > 60 {
				text = string([]rune(text)[:60]) + "…"
			}
			tip = "mCloudMount：" + text
			key := text + "|" + string(st.Phase) + "|" + map[bool]string{true: "1", false: "0"}[logged]
			if key == last {
				return
			}
			last = key
			systray.SetIcon(icons[state])
			systray.SetTooltip(tip)
			mStatus.SetTitle(text)
			busy := st.Phase == service.PhaseStarting || st.Phase == service.PhaseStopping
			if st.Phase == service.PhaseRunning {
				mToggle.SetTitle("卸载")
				mDrive.Enable()
			} else {
				mToggle.SetTitle("挂载")
				mDrive.Disable()
			}
			if busy || (!logged && st.Phase != service.PhaseRunning) {
				mToggle.Disable()
			} else {
				mToggle.Enable()
			}
		}
		refresh()

		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					refresh()
				case <-mPanel.ClickedCh:
					go a.OpenPanel()
				case <-mToggle.ClickedCh:
					if a.Status().Phase == service.PhaseRunning {
						go a.Unmount()
					} else {
						go a.Mount()
					}
				case <-mDrive.ClickedCh:
					go a.OpenDrive()
				case <-mQuit.ClickedCh:
					go a.Quit()
				}
			}
		}()
	}, func() {
		running.Store(false)
		close(stop)
	})
}

// Stop 移除托盘图标，使 Run 返回。
func Stop() { systray.Quit() }
