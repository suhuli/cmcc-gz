package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"mcloudmount/internal/cloud"
	"mcloudmount/internal/config"
	"mcloudmount/internal/logx"
	"mcloudmount/internal/panel"
	"mcloudmount/internal/platform"
	"mcloudmount/internal/service"
	"mcloudmount/internal/tray"
)

func logFile() string { return filepath.Join(config.Dir(), "logs", "mcloudmount.log") }

func fatal(format string, a ...any) int {
	msg := fmt.Sprintf(format, a...)
	slog.Error(msg)
	platform.Alert("mCloudMount", msg)
	return 1
}

// runApp 启动常驻模式：控制面板 + 托盘 + 可选自动挂载。
func runApp(args []string) int {
	fl := flag.NewFlagSet("mcloudmount", flag.ContinueOnError)
	background := fl.Bool("background", false, "后台启动，不打开浏览器")
	noBrowser := fl.Bool("no-browser", false, "不自动打开浏览器")
	verbose := fl.Bool("verbose", false, "同时在控制台输出日志")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *verbose {
		attachConsole()
	}

	store, err := config.Open("")
	if err != nil {
		return fatal("读取配置失败: %v", err)
	}
	cfg := store.Get()
	ring := logx.Setup(logx.Options{Level: cfg.LogLevel, File: logFile(), Console: *verbose})
	slog.Info("mCloudMount 启动", "version", version, "config", store.Path())

	port := cfg.PanelPort
	if port <= 0 {
		port = 8390
	}
	openBrowser := !*background && !*noBrowser

	client := cloud.New(store)
	svc := service.New(store, client)

	var quitOnce sync.Once
	quitCh := make(chan struct{})
	quit := func() { quitOnce.Do(func() { close(quitCh) }) }

	p := panel.New(panel.Options{
		Store: store, Service: svc, Logs: ring, Version: version,
		TrayAvailable: tray.Running, Quit: quit,
	})
	if err := p.Listen(port); err != nil {
		// 单实例：已有实例在运行时把它的面板打开即可
		if panel.Probe(port) {
			slog.Info("程序已在运行，打开现有控制面板")
			if !*background {
				_ = platform.OpenURL(fmt.Sprintf("http://127.0.0.1:%d/", port))
			}
			return 0
		}
		return fatal("控制面板端口 %d 被其他程序占用，无法启动。\n可在 %s 中修改 panel_port。", port, store.Path())
	}
	go func() {
		if err := p.Serve(); err != nil {
			slog.Error("控制面板异常退出", "error", err)
		}
	}()
	slog.Info("控制面板已启动", "url", p.URL())
	platform.RefreshAutostart()

	acct := store.Account()
	if cfg.AutoMount && acct.LoggedIn() && !acct.Expired(time.Now()) {
		go func() {
			// 开机自启时网络可能尚未就绪，失败后重试几次
			for i := 0; i < 4; i++ {
				err := svc.Start(context.Background(), service.Options{})
				if err == nil || svc.Status().Phase == service.PhaseIdle {
					return
				}
				select {
				case <-quitCh:
					return
				case <-time.After(time.Duration(10*(i+1)) * time.Second):
				}
				slog.Info("重试自动挂载")
			}
		}()
	}
	if openBrowser {
		_ = platform.OpenURL(p.URL())
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			quit()
		case <-quitCh:
		}
	}()

	actions := tray.Actions{
		OpenPanel: func() { _ = platform.OpenURL(p.URL()) },
		Mount: func() {
			if err := svc.Start(context.Background(), service.Options{}); err != nil {
				_ = platform.OpenURL(p.URL())
			}
		},
		Unmount: func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_ = svc.Stop(ctx)
		},
		OpenDrive: func() {
			if st := svc.Status(); st.Mounted {
				_ = platform.OpenPath(st.Drive + `\`)
			}
		},
		Quit:     quit,
		Status:   svc.Status,
		LoggedIn: func() bool { return store.Account().LoggedIn() },
	}

	shutdown := func() {
		slog.Info("正在退出…")
		svc.Shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		p.Shutdown(ctx)
		cancel()
		slog.Info("已退出")
	}

	if tray.Supported() {
		go func() {
			<-quitCh
			tray.Stop()
		}()
		tray.Run(actions) // 阻塞直到 tray.Stop
		quit()
	} else {
		<-quitCh
	}
	shutdown()
	return 0
}

// ---------------------------------------------------------------- 与正在运行的实例通信

var errNoInstance = errors.New("后台服务未运行")
