// Package panel 提供本机控制面板（http://127.0.0.1:8390）。
//
// 安全措施：只监听 127.0.0.1；校验 Host 头（防 DNS 重绑定）；所有修改类请求必须带 X-MCM 头
// （跨站页面无法在不触发预检的情况下设置自定义头，而本服务不响应 CORS）并校验 Origin。
package panel

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"mcloudmount/internal/cloud"
	"mcloudmount/internal/config"
	"mcloudmount/internal/icon"
	"mcloudmount/internal/logx"
	"mcloudmount/internal/platform"
	"mcloudmount/internal/service"
)

//go:embed index.html
var indexHTML []byte

// Options 是面板参数。
type Options struct {
	Store   *config.Store
	Service *service.Service
	Logs    *logx.Ring
	Version string
	// TrayAvailable 表示托盘图标是否在运行。
	TrayAvailable func() bool
	// Quit 退出整个程序（由 /api/quit 调用）。
	Quit func()
}

// Panel 是控制面板 HTTP 服务。
type Panel struct {
	o      Options
	client *cloud.Client
	port   int

	mu           sync.Mutex
	pendingPhone string
	lastSMS      map[string]time.Time

	srv *http.Server
	ln  net.Listener
}

// New 创建面板。
func New(o Options) *Panel {
	return &Panel{o: o, client: o.Service.Client(), lastSMS: map[string]time.Time{}}
}

// Listen 在 127.0.0.1:port 上监听。端口被占用时返回错误。
func (p *Panel) Listen(port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	p.ln, p.port = ln, ln.Addr().(*net.TCPAddr).Port
	return nil
}

// URL 返回面板地址。
func (p *Panel) URL() string { return fmt.Sprintf("http://127.0.0.1:%d/", p.port) }

// Serve 开始提供服务（阻塞直到 Shutdown）。
func (p *Panel) Serve() error {
	p.srv = &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 15 * time.Second}
	err := p.srv.Serve(p.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 停止面板服务。
func (p *Panel) Shutdown(ctx context.Context) {
	if p.srv != nil {
		_ = p.srv.Shutdown(ctx)
	}
}

// Probe 检查 port 上是否已经运行着本程序的面板。
func Probe(port int) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && resp.Header.Get("X-MCM-App") == "mcloudmount"
}

// ---------------------------------------------------------------- 路由与安全

// Handler 返回 HTTP 处理器（测试时可直接使用）。
func (p *Panel) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", p.index)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) { serveIcon(w, "image/x-icon", faviconICO) })
	mux.HandleFunc("GET /favicon.png", func(w http.ResponseWriter, r *http.Request) { serveIcon(w, "image/png", faviconPNG) })
	mux.HandleFunc("GET /api/status", p.status)
	mux.HandleFunc("POST /api/mount", p.mount)
	mux.HandleFunc("POST /api/unmount", p.unmount)
	mux.HandleFunc("POST /api/open-drive", p.openDrive)
	mux.HandleFunc("POST /api/login/send", p.loginSend)
	mux.HandleFunc("POST /api/login/verify", p.loginVerify)
	mux.HandleFunc("POST /api/logout", p.logout)
	mux.HandleFunc("GET /api/files", p.files)
	mux.HandleFunc("POST /api/files/rename", p.rename)
	mux.HandleFunc("POST /api/files/mkdir", p.mkdir)
	mux.HandleFunc("POST /api/files/delete", p.delete)
	mux.HandleFunc("GET /api/files/download", p.download)
	mux.HandleFunc("POST /api/files/upload", p.upload)
	mux.HandleFunc("GET /api/settings", p.getSettings)
	mux.HandleFunc("POST /api/settings", p.postSettings)
	mux.HandleFunc("GET /api/webclient", p.webclient)
	mux.HandleFunc("POST /api/webclient/fix", p.webclientFix)
	mux.HandleFunc("GET /api/logs", p.logs)
	mux.HandleFunc("POST /api/quit", p.quit)
	return p.guard(mux)
}

func (p *Panel) allowedHost(host string) bool {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	if p.port != 0 && port != strconv.Itoa(p.port) {
		return false
	}
	switch strings.ToLower(h) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func (p *Panel) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("面板请求异常", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeErr(w, http.StatusInternalServerError, "内部错误")
			}
		}()
		h := w.Header()
		h.Set("X-MCM-App", "mcloudmount")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if !p.allowedHost(r.Host) {
			writeErr(w, http.StatusForbidden, "禁止访问")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-MCM") != "1" {
				writeErr(w, http.StatusForbidden, "缺少请求头")
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !p.allowedHost(u.Host) {
					writeErr(w, http.StatusForbidden, "来源不被允许")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// cloudErr 把云端错误转换成合适的 HTTP 状态码与提示。
func cloudErr(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	switch {
	case cloud.IsKind(err, cloud.KindAuth):
		code = http.StatusUnauthorized
	case cloud.IsKind(err, cloud.KindNotFound):
		code = http.StatusNotFound
	case cloud.IsKind(err, cloud.KindConflict):
		code = http.StatusConflict
	case cloud.IsKind(err, cloud.KindRateLimited):
		code = http.StatusTooManyRequests
	}
	writeErr(w, code, cloud.UserMessage(err))
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return errors.New("请求格式错误")
	}
	return nil
}

var ok = map[string]bool{"ok": true}

func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	_, _ = w.Write(indexHTML)
}

var (
	faviconICO = icon.ICO(icon.Idle, 16, 32, 48)
	faviconPNG = icon.PNG(72, icon.Idle)
)

func serveIcon(w http.ResponseWriter, ctype string, data []byte) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "max-age=86400")
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------- 状态与挂载

func maskPhone(s string) string {
	if len(s) == 11 {
		return s[:3] + "****" + s[7:]
	}
	return s
}

func (p *Panel) status(w http.ResponseWriter, r *http.Request) {
	acct := p.o.Store.Account()
	st := p.o.Service.Status()
	p.mu.Lock()
	pending := p.pendingPhone
	p.mu.Unlock()
	name := acct.Account
	if name == "" || name == acct.Phone {
		name = maskPhone(acct.Phone)
	}
	var expire int64
	if acct.TokenExpireMs > 0 {
		expire = acct.TokenExpireMs
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"logged_in":       acct.LoggedIn(),
		"account":         name,
		"token_expired":   acct.LoggedIn() && acct.Expired(time.Now()),
		"token_expire_ms": expire,
		"drive":           st.Drive,
		"mounted":         st.Mounted,
		"server_running":  st.ServerRunning,
		"webdav":          st.WebDAV,
		"phase":           st.Phase,
		"message":         st.Message,
		"error":           st.Error,
		"since":           st.Since,
		"pending_phone":   pending,
		"mount_supported": platform.Supported(),
		"version":         p.o.Version,
		"uploads":         st.Uploads,
	})
}

func (p *Panel) mount(w http.ResponseWriter, r *http.Request) {
	acct := p.o.Store.Account()
	if !acct.LoggedIn() {
		writeErr(w, http.StatusBadRequest, "请先登录")
		return
	}
	if acct.Expired(time.Now()) {
		writeErr(w, http.StatusBadRequest, "登录已过期，请重新登录")
		return
	}
	st := p.o.Service.Status()
	if st.Phase == service.PhaseStarting || st.Phase == service.PhaseStopping {
		writeErr(w, http.StatusConflict, "正在处理中，请稍候")
		return
	}
	go func() { _ = p.o.Service.Start(context.Background(), service.Options{}) }()
	writeJSON(w, http.StatusAccepted, ok)
}

func (p *Panel) unmount(w http.ResponseWriter, r *http.Request) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = p.o.Service.Stop(ctx)
	}()
	writeJSON(w, http.StatusAccepted, ok)
}

func (p *Panel) openDrive(w http.ResponseWriter, r *http.Request) {
	st := p.o.Service.Status()
	var err error
	switch {
	case st.Mounted:
		err = platform.OpenPath(st.Drive + `\`)
	case st.ServerRunning && !platform.Supported():
		err = platform.OpenURL(st.WebDAV)
	default:
		writeErr(w, http.StatusBadRequest, "云盘尚未挂载")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "打开失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ok)
}

// ---------------------------------------------------------------- 登录

func (p *Panel) loginSend(w http.ResponseWriter, r *http.Request) {
	var req struct{ Phone string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	phone := strings.TrimSpace(req.Phone)
	if !cloud.ValidPhone(phone) {
		writeErr(w, http.StatusBadRequest, "请输入 11 位中国大陆手机号")
		return
	}
	p.mu.Lock()
	if last, okk := p.lastSMS[phone]; okk && time.Since(last) < 60*time.Second {
		wait := 60 - int(time.Since(last).Seconds())
		p.mu.Unlock()
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("发送太频繁，请 %d 秒后再试", wait))
		return
	}
	p.lastSMS[phone] = time.Now()
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := p.client.SendSMSCode(ctx, phone); err != nil {
		p.mu.Lock()
		delete(p.lastSMS, phone)
		p.mu.Unlock()
		slog.Warn("发送验证码失败", "error", err)
		cloudErr(w, err)
		return
	}
	p.mu.Lock()
	p.pendingPhone = phone
	p.mu.Unlock()
	slog.Info("验证码已发送", "phone", maskPhone(phone))
	writeJSON(w, http.StatusOK, ok)
}

func (p *Panel) loginVerify(w http.ResponseWriter, r *http.Request) {
	var req struct{ Phone, Code string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	phone, code := strings.TrimSpace(req.Phone), strings.TrimSpace(req.Code)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := p.client.Login(ctx, phone, code); err != nil {
		slog.Warn("登录失败", "error", err)
		cloudErr(w, err)
		return
	}
	p.mu.Lock()
	p.pendingPhone = ""
	p.mu.Unlock()
	slog.Info("登录成功", "phone", maskPhone(phone))
	writeJSON(w, http.StatusOK, ok)
}

func (p *Panel) logout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := p.o.Service.Stop(ctx); err != nil {
		slog.Warn("退出登录时停止挂载失败", "error", err)
	}
	if err := Logout(p.o.Store); err != nil {
		writeErr(w, http.StatusInternalServerError, "清除登录信息失败: "+err.Error())
		return
	}
	slog.Info("已退出登录")
	writeJSON(w, http.StatusOK, ok)
}

// Logout 清除本机保存的登录信息。
func Logout(store *config.Store) error {
	return store.Update(func(c *config.Config) error {
		c.Account = config.Account{ServerInfo: map[string]any{}, ExtInfo: map[string]any{}}
		return nil
	})
}

// ---------------------------------------------------------------- 文件

func (p *Panel) requireLogin(w http.ResponseWriter) bool {
	acct := p.o.Store.Account()
	if !acct.LoggedIn() {
		writeErr(w, http.StatusUnauthorized, "请先登录")
		return false
	}
	return true
}

// validName 检查云盘文件名是否合法。
func validName(name string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "" || name == "." || name == "..":
		return errors.New("名称不能为空")
	case utf8.RuneCountInString(name) > 255:
		return errors.New("名称太长")
	case strings.ContainsAny(name, `\/:*?"<>|`):
		return errors.New(`名称不能包含 \ / : * ? " < > |`)
	case strings.HasPrefix(name, "__mcm_"):
		return errors.New("名称不能以 __mcm_ 开头")
	}
	for _, r := range name {
		if r < 0x20 {
			return errors.New("名称包含非法字符")
		}
	}
	return nil
}

func (p *Panel) files(w http.ResponseWriter, r *http.Request) {
	if !p.requireLogin(w) {
		return
	}
	folder := r.URL.Query().Get("folder")
	if folder == "" {
		folder = cloud.RootID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	items, err := p.client.ListFolder(ctx, folder)
	if err != nil {
		cloudErr(w, err)
		return
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if strings.HasPrefix(it.Name, "__mcm_") {
			continue
		}
		var updated any = it.UpdatedRaw
		if !it.Updated.IsZero() {
			updated = it.Updated.UnixMilli()
		}
		out = append(out, map[string]any{"id": it.ID, "name": it.Name, "is_dir": it.IsDir, "size": it.Size, "updated": updated})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (p *Panel) rename(w http.ResponseWriter, r *http.Request) {
	if !p.requireLogin(w) {
		return
	}
	var req struct{ ID, Name, Parent string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validName(req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ID == "" || req.ID == cloud.RootID {
		writeErr(w, http.StatusBadRequest, "无效的文件")
		return
	}
	if err := p.client.Rename(r.Context(), req.ID, strings.TrimSpace(req.Name)); err != nil {
		cloudErr(w, err)
		return
	}
	p.o.Service.InvalidateDir(req.Parent)
	writeJSON(w, http.StatusOK, ok)
}

func (p *Panel) mkdir(w http.ResponseWriter, r *http.Request) {
	if !p.requireLogin(w) {
		return
	}
	var req struct{ Parent, Name string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validName(req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Parent == "" {
		req.Parent = cloud.RootID
	}
	if _, err := p.client.CreateFolder(r.Context(), req.Parent, strings.TrimSpace(req.Name)); err != nil {
		cloudErr(w, err)
		return
	}
	p.o.Service.InvalidateDir(req.Parent)
	writeJSON(w, http.StatusOK, ok)
}

func (p *Panel) delete(w http.ResponseWriter, r *http.Request) {
	if !p.requireLogin(w) {
		return
	}
	var req struct {
		IDs    []string `json:"ids"`
		Parent string   `json:"parent"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ids := req.IDs[:0]
	for _, id := range req.IDs {
		if id != "" && id != cloud.RootID {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		writeErr(w, http.StatusBadRequest, "没有选择文件")
		return
	}
	for start := 0; start < len(ids); start += 100 {
		end := min(start+100, len(ids))
		if err := p.client.Trash(r.Context(), ids[start:end]); err != nil {
			p.o.Service.InvalidateDir(req.Parent)
			cloudErr(w, err)
			return
		}
	}
	p.o.Service.InvalidateDir(req.Parent)
	slog.Info("已删除文件", "count", len(ids))
	writeJSON(w, http.StatusOK, ok)
}

// download 通过本机中转下载，确保文件名正确（含中文）。
func (p *Panel) download(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeErr(w, http.StatusForbidden, "禁止跨站下载")
		return
	}
	if !p.requireLogin(w) {
		return
	}
	q := r.URL.Query()
	id, name := q.Get("id"), q.Get("name")
	if id == "" || id == cloud.RootID {
		writeErr(w, http.StatusBadRequest, "无效的文件")
		return
	}
	if name == "" || validName(name) != nil {
		name = "download"
	}
	body, err := p.client.OpenFile(r.Context(), id, 0)
	if err != nil {
		cloudErr(w, err)
		return
	}
	defer body.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	if size, err := strconv.ParseInt(q.Get("size"), 10, 64); err == nil && size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if _, err := io.Copy(w, body); err != nil && r.Context().Err() == nil {
		slog.Warn("下载中断", "name", name, "error", err)
	}
}

// upload 接收原始请求体（?parent=ID&name=文件名），暂存到本地后上传。
func (p *Panel) upload(w http.ResponseWriter, r *http.Request) {
	if !p.requireLogin(w) {
		return
	}
	q := r.URL.Query()
	parent, name := q.Get("parent"), strings.TrimSpace(q.Get("name"))
	if parent == "" {
		parent = cloud.RootID
	}
	if err := validName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.ContentLength < 0 {
		writeErr(w, http.StatusLengthRequired, "缺少 Content-Length")
		return
	}
	dir := filepath.Join(config.Dir(), "tmp")
	_ = os.MkdirAll(dir, 0o700)
	f, err := os.CreateTemp(dir, "upload-")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建临时文件失败: "+err.Error())
		return
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), r.Body)
	if err != nil || n != r.ContentLength {
		writeErr(w, http.StatusBadRequest, "上传数据不完整")
		return
	}
	// 上传不跟随浏览器连接取消：数据已经完整收到
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 6*time.Hour)
	defer cancel()
	res, err := p.client.Upload(ctx, cloud.UploadRequest{
		ParentID: parent, Name: name, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil)), Source: f,
	})
	if err != nil {
		slog.Warn("上传失败", "name", name, "error", err)
		cloudErr(w, err)
		return
	}
	p.o.Service.InvalidateDir(parent)
	slog.Info("上传完成", "name", res.Name, "size", n, "rapid", res.Rapid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": res.FileID, "name": res.Name, "rapid": res.Rapid})
}

// ---------------------------------------------------------------- 设置

func (p *Panel) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg := p.o.Store.Get()
	tray := false
	if p.o.TrayAvailable != nil {
		tray = p.o.TrayAvailable()
	}
	drives := platform.FreeDrives()
	if cfg.Mount.Drive != "" {
		found := false
		for _, d := range drives {
			found = found || strings.EqualFold(d, cfg.Mount.Drive)
		}
		if !found {
			drives = append([]string{cfg.Mount.Drive}, drives...)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"autostart":           platform.AutostartEnabled(),
		"autostart_supported": platform.AutostartSupported(),
		"auto_mount":          cfg.AutoMount,
		"tray_available":      tray,
		"mount_supported":     platform.Supported(),
		"drive":               cfg.Mount.Drive,
		"drives":              drives,
		"port":                cfg.Mount.Port,
		"webdav_user":         cfg.Mount.DavUser,
		"webdav_password":     cfg.Mount.DavPassword,
		"log_level":           cfg.LogLevel,
		"config_dir":          config.Dir(),
		"version":             p.o.Version,
	})
}

func (p *Panel) postSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Autostart *bool   `json:"autostart"`
		AutoMount *bool   `json:"auto_mount"`
		Drive     *string `json:"drive"`
		Port      *int    `json:"port"`
		LogLevel  *string `json:"log_level"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Autostart != nil {
		if err := platform.SetAutostart(*req.Autostart); err != nil {
			writeErr(w, http.StatusInternalServerError, "设置开机自启失败: "+err.Error())
			return
		}
	}
	if (req.Drive != nil || req.Port != nil) && p.o.Service.Running() {
		writeErr(w, http.StatusConflict, "请先卸载，再修改盘符或端口")
		return
	}
	if req.Drive != nil {
		d := strings.ToUpper(strings.TrimSpace(*req.Drive))
		if len(d) == 1 {
			d += ":"
		}
		if d == "" || !config.ValidDrive(d) {
			writeErr(w, http.StatusBadRequest, "盘符无效")
			return
		}
		req.Drive = &d
	}
	if req.Port != nil && (*req.Port < 1024 || *req.Port > 65535 || *req.Port == p.port) {
		writeErr(w, http.StatusBadRequest, "端口需在 1024–65535 之间，且不能与面板端口相同")
		return
	}
	err := p.o.Store.Update(func(c *config.Config) error {
		if req.AutoMount != nil {
			c.AutoMount = *req.AutoMount
		}
		if req.Drive != nil {
			c.Mount.Drive = *req.Drive
		}
		if req.Port != nil {
			c.Mount.Port = *req.Port
		}
		if req.LogLevel != nil {
			c.LogLevel = strings.ToUpper(*req.LogLevel)
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	if req.LogLevel != nil {
		logx.SetLevel(*req.LogLevel)
	}
	writeJSON(w, http.StatusOK, ok)
}

func (p *Panel) webclient(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, platform.GetWebClientInfo())
}

func (p *Panel) webclientFix(w http.ResponseWriter, r *http.Request) {
	if !platform.Supported() {
		writeErr(w, http.StatusBadRequest, "仅 Windows 需要此设置")
		return
	}
	code, err := platform.RunElevated("setup-webclient")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if code != 0 {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("设置未完成（退出码 %d），详情见日志文件", code))
		return
	}
	slog.Info("WebClient 设置已更新")
	writeJSON(w, http.StatusOK, platform.GetWebClientInfo())
}

func (p *Panel) logs(w http.ResponseWriter, r *http.Request) {
	var lines []string
	if p.o.Logs != nil {
		lines = p.o.Logs.Tail(300)
	}
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (p *Panel) quit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ok)
	if p.o.Quit != nil {
		go func() {
			time.Sleep(200 * time.Millisecond)
			p.o.Quit()
		}()
	}
}
