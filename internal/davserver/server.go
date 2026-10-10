// Package davserver 在本地端口提供 WebDAV 服务，供 Windows 映射为盘符。
package davserver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"mcloudmount/internal/davfs"
)

// Options 是服务参数。
type Options struct {
	Host     string
	Port     int
	User     string
	Password string
}

// Server 是 WebDAV 服务。
type Server struct {
	opts Options
	fs   *davfs.FS
	dav  *webdav.Handler
	auth *authenticator

	mu      sync.Mutex
	srv     *http.Server
	ln      net.Listener
	running bool
	done    chan struct{}
}

// New 创建服务（尚未监听）。
func New(fsys *davfs.FS, opts Options) *Server {
	s := &Server{opts: opts, fs: fsys, auth: newAuthenticator(opts.User, opts.Password, "mCloudMount")}
	s.dav = &webdav.Handler{
		FileSystem: fsys,
		LockSystem: newLenientLS(),
		Logger:     s.logRequest,
	}
	return s
}

func (s *Server) logRequest(r *http.Request, err error) {
	if err == nil {
		slog.Debug("webdav", "method", r.Method, "path", r.URL.Path)
		return
	}
	if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
		return // 资源管理器会探测大量不存在的文件（desktop.ini 等），不记录
	}
	slog.Warn("WebDAV 请求失败", "method", r.Method, "path", r.URL.Path, "error", err)
}

// Handler 返回完整的 HTTP 处理器（含认证），用于测试或自定义监听。
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// Start 监听端口并在后台提供服务。端口被占用时立即返回错误。
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	addr := net.JoinHostPort(s.opts.Host, strconv.Itoa(s.opts.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("WebDAV 服务无法监听 %s（端口可能被占用）: %w", addr, err)
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          nil,
	}
	s.done = make(chan struct{})
	s.running = true
	go func(srv *http.Server, done chan struct{}) {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("WebDAV 服务异常退出", "error", err)
		}
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}(s.srv, s.done)
	slog.Info("WebDAV 服务已启动", "addr", "http://"+addr)
	return nil
}

// Running 表示服务是否在运行。
func (s *Server) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Stop 优雅停止：等待进行中的请求（最多 timeout），然后关闭。
func (s *Server) Stop(timeout time.Duration) error {
	s.mu.Lock()
	srv, done := s.srv, s.done
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	<-done
	s.mu.Lock()
	s.srv, s.ln = nil, nil
	s.mu.Unlock()
	return err
}

func (s *Server) hostAllowed(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	switch h {
	case "127.0.0.1", "localhost", "::1", "":
		return true
	}
	return h == strings.ToLower(s.opts.Host)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if v := recover(); v != nil {
			slog.Error("WebDAV 处理异常", "method", r.Method, "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
			http.Error(w, "内部错误", http.StatusInternalServerError)
		}
	}()
	// 防 DNS 重绑定：只接受本机地址作为 Host
	if !s.hostAllowed(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	if !s.auth.check(w, r) {
		return
	}
	w.Header().Set("MS-Author-Via", "DAV")
	w.Header().Set("Cache-Control", "no-cache")

	// 锁只作兼容（见 lenientLS）。If 头仅用于 LOCK 刷新；其他请求忽略它，否则 x/net/webdav 会因
	// 令牌所带主机名不匹配（如 Windows 的 127.0.0.1@8380 写法）或格式问题直接返回 412/400。
	if r.Method != "LOCK" {
		r.Header.Del("If")
	}

	ctx := r.Context()
	switch r.Method {
	case http.MethodPut:
		g := davfs.NewTransferGuard(r.ContentLength)
		r.Body = g.WrapBody(r.Body)
		r = r.WithContext(davfs.WithGuard(ctx, g))
	case "COPY":
		r = r.WithContext(davfs.WithGuard(ctx, davfs.NewTransferGuard(-1)))
	case "MOVE":
		if s.caseOnlyMove(w, r) {
			return
		}
	case http.MethodGet, http.MethodHead:
		// 预先设置类型，避免 http.ServeContent 为了嗅探类型多读一次文件
		if t := mime.TypeByExtension(path.Ext(r.URL.Path)); t != "" {
			w.Header().Set("Content-Type", t)
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
	}
	s.dav.ServeHTTP(w, r)
}

// caseOnlyMove 处理只改变大小写的改名（a.txt -> A.txt）。
// 云盘按不区分大小写匹配时，webdav 会认为目标已存在并先删除它——也就是删掉源文件。
// 这里直接执行改名并返回结果。
func (s *Server) caseOnlyMove(w http.ResponseWriter, r *http.Request) bool {
	dest := r.Header.Get("Destination")
	if dest == "" {
		return false
	}
	u, err := url.Parse(dest)
	if err != nil {
		return false
	}
	src := path.Clean("/" + r.URL.Path)
	dst := path.Clean("/" + u.Path)
	if src == dst || !strings.EqualFold(src, dst) {
		return false
	}
	if err := s.fs.Rename(r.Context(), src, dst); err != nil {
		status := http.StatusForbidden
		if errors.Is(err, fs.ErrNotExist) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return true
	}
	w.WriteHeader(http.StatusCreated)
	return true
}
