// Package service 负责挂载流程的编排：登录检查 → 连接云端 → 启动 WebDAV → 映射盘符 → 健康检查。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"mcloudmount/internal/cloud"
	"mcloudmount/internal/config"
	"mcloudmount/internal/davfs"
	"mcloudmount/internal/davserver"
	"mcloudmount/internal/platform"
	"mcloudmount/internal/vfs"
)

// Phase 是服务所处阶段。
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseStarting Phase = "starting"
	PhaseRunning  Phase = "running"
	PhaseStopping Phase = "stopping"
	PhaseError    Phase = "error"
)

// Status 是服务状态快照。
type Status struct {
	Phase         Phase  `json:"phase"`
	Message       string `json:"message"`
	Error         string `json:"error"`
	Drive         string `json:"drive"`
	Mounted       bool   `json:"mounted"`
	ServerRunning bool   `json:"server_running"`
	WebDAV        string `json:"webdav"`
	Since         int64  `json:"since"`
}

// Options 控制启动行为。
type Options struct {
	// NoMount 只启动 WebDAV 服务，不映射盘符（非 Windows 系统总是如此）。
	NoMount bool
}

// Service 管理一次挂载的完整生命周期。并发安全。
type Service struct {
	store  *config.Store
	client *cloud.Client

	op sync.Mutex // 串行化 Start / Stop

	mu       sync.Mutex
	phase    Phase
	message  string
	errMsg   string
	since    time.Time
	vfs      *vfs.FS
	fs       *davfs.FS
	dav      *davserver.Server
	tmpDir   string
	settings config.MountSettings
	mapped   bool
	stopMon  chan struct{}
	monDone  chan struct{}

	// OnChange 在阶段变化时回调（可选，例如刷新托盘图标）。
	OnChange func(Status)
}

// New 创建服务。
func New(store *config.Store, client *cloud.Client) *Service {
	return &Service{store: store, client: client, phase: PhaseIdle}
}

// Client 返回共享的云盘客户端。
func (s *Service) Client() *cloud.Client { return s.client }

func (s *Service) set(p Phase, msg, errMsg string) {
	s.mu.Lock()
	if s.phase != p {
		s.since = time.Now()
	}
	s.phase, s.message, s.errMsg = p, msg, errMsg
	cb := s.OnChange
	s.mu.Unlock()
	switch {
	case errMsg != "":
		slog.Error(errMsg)
	case msg != "":
		slog.Info(msg)
	}
	if cb != nil {
		cb(s.Status())
	}
}

// Status 返回当前状态。
func (s *Service) Status() Status {
	s.mu.Lock()
	st := Status{Phase: s.phase, Message: s.message, Error: s.errMsg}
	if !s.since.IsZero() {
		st.Since = s.since.Unix()
	}
	dav, set, mapped := s.dav, s.settings, s.mapped
	s.mu.Unlock()
	if dav != nil && dav.Running() {
		st.ServerRunning = true
		st.WebDAV = "http://" + net.JoinHostPort(set.Host, strconv.Itoa(set.Port)) + "/"
	}
	if set.Drive == "" {
		set.Drive = s.store.Get().Mount.Drive
	}
	st.Drive = set.Drive
	if mapped {
		st.Mounted = platform.Mounted(set.Drive, set.Host, set.Port)
	}
	return st
}

// Running 表示服务是否处于运行（或启动中）状态。
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase == PhaseRunning || s.phase == PhaseStarting
}

// InvalidateDir 让某个目录的缓存失效（控制面板修改文件后调用，使资源管理器看到最新内容）。
func (s *Service) InvalidateDir(dirID string) {
	s.mu.Lock()
	v := s.vfs
	s.mu.Unlock()
	if v == nil {
		return
	}
	if dirID == "" {
		v.InvalidateAll()
		return
	}
	v.Invalidate(dirID)
}

// ensureDavPassword 首次运行时生成随机 WebDAV 密码。
func (s *Service) ensureDavPassword() (config.MountSettings, error) {
	err := s.store.Update(func(c *config.Config) error {
		if c.Mount.DavUser == "" {
			c.Mount.DavUser = "mcloud"
		}
		if c.Mount.DavPassword == "" {
			c.Mount.DavPassword = config.RandomString(24)
		}
		if c.Mount.Host == "" {
			c.Mount.Host = "127.0.0.1"
		}
		return nil
	})
	return s.store.Get().Mount, err
}

// Start 启动挂载。已经在运行时直接返回 nil。
func (s *Service) Start(ctx context.Context, opts Options) (err error) {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	if s.phase == PhaseRunning {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	s.set(PhaseStarting, "正在启动…", "")
	defer func() {
		if err != nil {
			s.cleanup(context.Background(), false)
			s.set(PhaseError, "", err.Error())
		}
	}()

	acct := s.store.Account()
	if !acct.LoggedIn() {
		return errors.New("尚未登录，请先登录")
	}
	if acct.Expired(time.Now()) {
		return errors.New("登录已过期，请重新登录")
	}
	set, err := s.ensureDavPassword()
	if err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}
	if set.Port <= 0 || set.Port > 65535 {
		return fmt.Errorf("WebDAV 端口无效: %d", set.Port)
	}
	mount := platform.Supported() && !opts.NoMount
	if mount && (set.Drive == "" || !config.ValidDrive(set.Drive)) {
		return fmt.Errorf("盘符无效: %q", set.Drive)
	}

	s.set(PhaseStarting, "正在连接云盘…", "")
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	profile, base, err := s.client.ResolveConnection(rctx)
	cancel()
	if err != nil {
		if cloud.IsKind(err, cloud.KindAuth) {
			return errors.New("登录已失效，请重新登录")
		}
		return fmt.Errorf("连接云盘失败: %s", cloud.UserMessage(err))
	}
	slog.Info("云盘连接成功", "profile", profile, "base", base)

	if mount {
		if err := platform.EnsureWebClient(ctx); err != nil {
			// 映射盘符时系统通常会自动拉起服务，这里只记录
			slog.Warn("WebClient 服务未运行", "error", err)
		}
		if platform.DriveInUse(set.Drive) {
			remote := platform.RemoteName(set.Drive)
			if !platform.IsOurMapping(remote, set.Host, set.Port) {
				if remote == "" {
					remote = "本地磁盘"
				}
				return fmt.Errorf("盘符 %s 已被占用（%s），请在设置中换一个盘符", set.Drive, remote)
			}
			slog.Info("清理上次遗留的盘符映射", "drive", set.Drive)
			if err := platform.Unmount(ctx, set.Drive); err != nil {
				platform.ForceUnmount(set.Drive)
			}
		}
	}

	tmp, err := prepareTmp()
	if err != nil {
		return err
	}
	v := vfs.New(s.client)
	fsys, err := davfs.New(s.client, v, tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("初始化文件系统失败: %w", err)
	}
	fsys.OnChange = func(op, p string) { slog.Info("云盘变更", "op", op, "path", p) }
	dav := davserver.New(fsys, davserver.Options{Host: set.Host, Port: set.Port, User: set.DavUser, Password: set.DavPassword})
	s.mu.Lock()
	s.vfs, s.fs, s.dav, s.tmpDir, s.settings, s.mapped = v, fsys, dav, tmp, set, false
	s.mu.Unlock()

	if err := dav.Start(); err != nil {
		return err
	}
	slog.Info("WebDAV 服务已启动", "addr", dav.Addr())

	if mount {
		s.set(PhaseStarting, "正在映射盘符 "+set.Drive+"…", "")
		if err := platform.Mount(ctx, set.Drive, set.Host, set.Port, set.DavUser, set.DavPassword); err != nil {
			return err
		}
		s.mu.Lock()
		s.mapped = true
		s.mu.Unlock()
	}

	s.startMonitor()
	if mount {
		s.set(PhaseRunning, "已挂载到 "+set.Drive, "")
	} else {
		s.set(PhaseRunning, "WebDAV 服务运行中："+dav.Addr(), "")
	}
	return nil
}

// Stop 停止挂载：断开盘符 → 停止 WebDAV（等待传输完成）→ 关闭文件系统。
func (s *Service) Stop(ctx context.Context) error {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	idle := s.phase == PhaseIdle || (s.phase == PhaseError && s.dav == nil)
	s.mu.Unlock()
	if idle {
		s.set(PhaseIdle, "", "")
		return nil
	}
	s.set(PhaseStopping, "正在停止…", "")
	err := s.cleanup(ctx, true)
	if err != nil {
		s.set(PhaseError, "", err.Error())
		return err
	}
	s.set(PhaseIdle, "已停止", "")
	return nil
}

// cleanup 释放所有资源。调用方须持有 op 锁。
func (s *Service) cleanup(ctx context.Context, graceful bool) error {
	s.stopMonitor()
	s.mu.Lock()
	fsys, dav, tmp, set, mapped := s.fs, s.dav, s.tmpDir, s.settings, s.mapped
	s.mu.Unlock()

	var firstErr error
	if mapped {
		uctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := platform.Unmount(uctx, set.Drive)
		cancel()
		if err != nil {
			if graceful {
				slog.Warn("正常断开失败，强制断开", "error", err)
			}
			platform.ForceUnmount(set.Drive)
		}
	}
	if dav != nil {
		if err := dav.Stop(10 * time.Second); err != nil {
			slog.Warn("停止 WebDAV 服务", "error", err)
		}
	}
	if fsys != nil {
		if err := fsys.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("关闭文件系统: %w", err)
		}
	}
	if tmp != "" {
		os.RemoveAll(tmp)
	}
	s.mu.Lock()
	s.vfs, s.fs, s.dav, s.tmpDir, s.mapped = nil, nil, nil, "", false
	s.mu.Unlock()
	return firstErr
}

// Shutdown 退出程序时调用，尽量快地释放资源。
func (s *Service) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = s.Stop(ctx)
}

// ---------------------------------------------------------------- 健康检查

const monitorInterval = 15 * time.Second

func (s *Service) startMonitor() {
	stop, done := make(chan struct{}), make(chan struct{})
	s.mu.Lock()
	s.stopMon, s.monDone = stop, done
	s.mu.Unlock()
	go s.monitor(stop, done)
}

func (s *Service) stopMonitor() {
	s.mu.Lock()
	stop, done := s.stopMon, s.monDone
	s.stopMon, s.monDone = nil, nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}

func (s *Service) monitor(stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(monitorInterval)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		dav, set, mapped := s.dav, s.settings, s.mapped
		s.mu.Unlock()
		if dav == nil || !dav.Running() {
			go s.fail("WebDAV 服务意外停止")
			return
		}
		if s.store.Account().Expired(time.Now()) {
			go s.fail("登录已过期，请重新登录")
			return
		}
		if !mapped {
			continue
		}
		if platform.Mounted(set.Drive, set.Host, set.Port) {
			misses = 0
			continue
		}
		misses++
		if misses < 2 {
			continue
		}
		slog.Warn("盘符映射丢失，尝试重新映射", "drive", set.Drive)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err := platform.Mount(ctx, set.Drive, set.Host, set.Port, set.DavUser, set.DavPassword)
		cancel()
		if err != nil {
			go s.fail("盘符 " + set.Drive + " 已断开且无法重新映射: " + err.Error())
			return
		}
		misses = 0
		slog.Info("已重新映射盘符", "drive", set.Drive)
	}
}

// fail 在健康检查发现异常时停止服务并进入错误状态。
func (s *Service) fail(msg string) {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	running := s.phase == PhaseRunning
	s.mu.Unlock()
	if !running {
		return
	}
	s.cleanup(context.Background(), false)
	s.set(PhaseError, "", msg)
}

// ---------------------------------------------------------------- 临时目录

// prepareTmp 为本次运行创建独立的暂存目录，并清理之前异常退出遗留的目录。
func prepareTmp() (string, error) {
	base := filepath.Join(config.Dir(), "tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	if ents, err := os.ReadDir(base); err == nil {
		for _, e := range ents {
			if !strings.HasPrefix(e.Name(), "run-") {
				continue
			}
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				os.RemoveAll(filepath.Join(base, e.Name()))
			}
		}
	}
	dir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	return dir, nil
}
