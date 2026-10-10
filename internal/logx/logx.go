// Package logx 提供日志输出：内存环形缓冲（供控制面板显示）+ 滚动文件日志 + 可选控制台。
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Ring 保存最近的日志行。
type Ring struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

// NewRing 创建容量为 n 的缓冲。
func NewRing(n int) *Ring { return &Ring{lines: make([]string, n)} }

func (r *Ring) add(line string) {
	r.mu.Lock()
	r.lines[r.next] = line
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Tail 返回最近 n 行（旧的在前）。
func (r *Ring) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []string
	if r.full {
		all = append(all, r.lines[r.next:]...)
	}
	all = append(all, r.lines[:r.next]...)
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// rotatingFile 是按大小滚动的日志文件。
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func openRotating(path string, max int64) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	r := &rotatingFile{path: path, max: max}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	r.f = f
	if st != nil {
		r.size = st.Size()
	}
	return nil
}

// Close 关闭日志文件。
func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return len(p), nil
	}
	if r.size+int64(len(p)) > r.max {
		_ = r.f.Close()
		_ = os.Remove(r.path + ".2")
		_ = os.Rename(r.path+".1", r.path+".2")
		_ = os.Rename(r.path, r.path+".1")
		if err := r.open(); err != nil {
			r.f = nil
			return len(p), nil
		}
		r.size = 0
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// handler 是简洁的文本格式 slog 处理器：“15:04:05 INFO 消息 key=value”。
type handler struct {
	level  *slog.LevelVar
	ring   *Ring
	out    []io.Writer
	attrs  []slog.Attr
	groups []string
	mu     *sync.Mutex
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func (h *handler) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(levelName(rec.Level))
	b.WriteByte(' ')
	b.WriteString(rec.Message)
	write := func(a slog.Attr) {
		if a.Equal(slog.Attr{}) {
			return
		}
		b.WriteByte(' ')
		if len(h.groups) > 0 {
			b.WriteString(strings.Join(h.groups, ".") + ".")
		}
		b.WriteString(a.Key)
		b.WriteByte('=')
		v := a.Value.Resolve().String()
		if strings.ContainsAny(v, " \t\"") {
			v = fmt.Sprintf("%q", v)
		}
		b.WriteString(v)
	}
	for _, a := range h.attrs {
		write(a)
	}
	rec.Attrs(func(a slog.Attr) bool { write(a); return true })
	msg := b.String()
	t := rec.Time
	if t.IsZero() {
		t = time.Now()
	}
	h.ring.add(t.Format("15:04:05") + " " + msg)
	if len(h.out) > 0 {
		line := t.Format("2006-01-02 15:04:05") + " " + msg + "\n"
		h.mu.Lock()
		for _, w := range h.out {
			_, _ = io.WriteString(w, line)
		}
		h.mu.Unlock()
	}
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	c := *h
	c.groups = append(append([]string{}, h.groups...), name)
	return &c
}

// Options 控制日志输出。
type Options struct {
	Level   string // DEBUG / INFO / WARNING / ERROR
	File    string // 日志文件路径，空表示不写文件
	Console bool   // 同时输出到标准错误
}

// Setup 安装全局日志并返回内存缓冲。
func Setup(o Options) *Ring {
	ring := NewRing(400)
	lv := level
	lv.Set(ParseLevel(o.Level))
	h := &handler{level: lv, ring: ring, mu: &sync.Mutex{}}
	fileMu.Lock()
	if current != nil {
		_ = current.Close()
		current = nil
	}
	if o.File != "" {
		if f, err := openRotating(o.File, 2<<20); err == nil {
			h.out = append(h.out, f)
			current = f
		}
	}
	fileMu.Unlock()
	if o.Console {
		h.out = append(h.out, os.Stderr)
	}
	slog.SetDefault(slog.New(h))
	return ring
}

var (
	level   = &slog.LevelVar{}
	fileMu  sync.Mutex
	current *rotatingFile
)

// Close 关闭日志文件（之后的日志只写入内存与控制台）。
func Close() {
	fileMu.Lock()
	defer fileMu.Unlock()
	if current != nil {
		_ = current.Close()
		current = nil
	}
}

// SetLevel 运行时修改日志级别。
func SetLevel(s string) { level.Set(ParseLevel(s)) }

// ParseLevel 解析日志级别文本。
func ParseLevel(s string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	}
	return slog.LevelInfo
}
