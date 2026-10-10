package davfs

import (
	"context"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"mcloudmount/internal/vfs"
)

// ---------------------------------------------------------------- 传输保护

type guardKey struct{}

// TransferGuard 记录一次请求中的传输是否完整。
// PUT 时由 HTTP 层包装请求体；COPY 时由读取句柄报告读取错误。
// 写入句柄关闭时如果发现传输不完整，就放弃上传，绝不用残缺内容覆盖云端文件。
type TransferGuard struct {
	expected int64 // 期望字节数，-1 表示未知
	n        atomic.Int64
	mu       sync.Mutex
	err      error
}

// NewTransferGuard 创建保护对象。expected<0 表示长度未知。
func NewTransferGuard(expected int64) *TransferGuard { return &TransferGuard{expected: expected} }

// WithGuard 把保护对象放入上下文。
func WithGuard(ctx context.Context, g *TransferGuard) context.Context {
	return context.WithValue(ctx, guardKey{}, g)
}

func guardFrom(ctx context.Context) *TransferGuard {
	g, _ := ctx.Value(guardKey{}).(*TransferGuard)
	return g
}

func (g *TransferGuard) fail(err error) {
	if g == nil || err == nil {
		return
	}
	g.mu.Lock()
	if g.err == nil {
		g.err = err
	}
	g.mu.Unlock()
}

// problem 返回传输问题；完整时返回 nil。
func (g *TransferGuard) problem() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	err := g.err
	g.mu.Unlock()
	if err != nil {
		return err
	}
	if g.expected >= 0 && g.n.Load() != g.expected {
		return fmt.Errorf("传输不完整：期望 %d 字节，收到 %d 字节", g.expected, g.n.Load())
	}
	return nil
}

// WrapBody 包装 PUT 请求体，记录读取字节数与错误。
func (g *TransferGuard) WrapBody(rc io.ReadCloser) io.ReadCloser { return &guardedBody{rc: rc, g: g} }

type guardedBody struct {
	rc io.ReadCloser
	g  *TransferGuard
}

func (b *guardedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.g.n.Add(int64(n))
	if err != nil && err != io.EOF {
		b.g.fail(err)
	}
	return n, err
}

func (b *guardedBody) Close() error { return b.rc.Close() }

// ---------------------------------------------------------------- 写入句柄

type writeFile struct {
	fs          *FS
	path        string
	parent      vfs.Entry
	name        string
	existing    *vfs.Entry
	placeholder *placeholder
	ctx         context.Context
	guard       *TransferGuard
	unlock      func()

	tmp      *os.File
	hasher   hash.Hash
	size     int64
	pos      int64
	seqOK    bool // 写入是否严格顺序（可以直接使用增量哈希）
	writeErr error
	mod      time.Time
	closed   bool
	truncate bool // 打开时要求清空文件（O_TRUNC）
	wrote    bool // 是否收到过写入
}

func (w *writeFile) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fs.ErrClosed
	}
	w.wrote = true
	if w.pos != w.size {
		w.seqOK = false
	}
	n, err := w.tmp.Write(p)
	if w.seqOK {
		w.hasher.Write(p[:n])
	}
	w.pos += int64(n)
	if w.pos > w.size {
		w.size = w.pos
	}
	if err != nil {
		w.writeErr = err
	}
	return n, err
}

func (w *writeFile) Seek(offset int64, whence int) (int64, error) {
	pos, err := w.tmp.Seek(offset, whence)
	if err == nil {
		w.pos = pos
	}
	return pos, err
}

func (w *writeFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: w.path, Err: fs.ErrPermission}
}

func (w *writeFile) Readdir(int) ([]fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "readdir", Path: w.path, Err: ErrNotDir}
}

func (w *writeFile) Stat() (fs.FileInfo, error) {
	return &fileInfo{name: w.name, size: w.size, mod: w.mod, etag: fmt.Sprintf(`"w-%d-%d"`, w.size, w.mod.UnixNano())}, nil
}

func (w *writeFile) DeadProps() (map[xmlName]Property, error) {
	return w.fs.props.get(key(w.path)), nil
}
func (w *writeFile) Patch(p []Proppatch) ([]Propstat, error) {
	return w.fs.props.patch(key(w.path), p), nil
}

// Close 校验传输完整性后把内容放入上传队列并立即返回；真正的云端上传在后台进行。
//
// 早期版本在 Close 里同步上传，Windows WebClient 要等到云端上传结束才收到 PUT 的响应，
// 文件稍大或网络稍慢就会超时，资源管理器报“网络错误”。
func (w *writeFile) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer w.unlock()
	keep := false
	defer func() {
		name := w.tmp.Name()
		_ = w.tmp.Close()
		if !keep {
			_ = os.Remove(name)
		}
	}()

	problem := w.writeErr
	if problem == nil {
		problem = w.guard.problem()
	}
	if problem == nil && w.ctx.Err() != nil {
		problem = w.ctx.Err()
	}
	if problem != nil {
		slog.Warn("传输未完成，已放弃上传（云端文件保持不变）", "path", w.path, "reason", problem)
		w.restorePlaceholder()
		return &fs.PathError{Op: "close", Path: w.path, Err: problem}
	}

	// 既没有写入、也没有要求清空：保持原内容不变（保险措施，防止误覆盖）
	if !w.wrote && !w.truncate && (w.existing != nil || w.placeholder != nil) {
		w.restorePlaceholder()
		return nil
	}
	old := w.placeholder
	now := time.Now()
	p := &placeholder{path: w.path, parentID: w.parent.ID, name: w.name, created: now, size: w.size}
	if w.size == 0 {
		switch {
		case w.existing == nil && (old == nil || !old.content()):
			// 新建空文件：延迟创建，通常紧接着就会写入真正的内容
			p.sum = emptySHA256
			w.fs.pending.put(key(w.path), p, w.fs, w.fs.EmptyFileDelay)
			return nil
		case w.existing != nil && w.existing.Size == 0:
			return nil
		}
		// 把已有文件清空：需要替换云端内容
		p.sum, p.replace = emptySHA256, true
	} else {
		if w.seqOK {
			p.sum = hex.EncodeToString(w.hasher.Sum(nil))
		} else {
			w.hasher.Reset()
			if _, err := io.Copy(w.hasher, io.NewSectionReader(w.tmp, 0, w.size)); err != nil {
				w.restorePlaceholder()
				return &fs.PathError{Op: "close", Path: w.path, Err: err}
			}
			p.sum = hex.EncodeToString(w.hasher.Sum(nil))
		}
		p.local, p.replace = w.tmp.Name(), true
		keep = true
	}
	if old != nil && old.local != "" && old.local != p.local {
		w.fs.removeLater(old.local)
	}
	w.fs.pending.put(key(w.path), p, w.fs, w.fs.UploadDelay)
	w.fs.persist()
	slog.Info("已接收文件，后台上传中", "path", w.path, "size", w.size)
	return nil
}

func (w *writeFile) restorePlaceholder() {
	if w.placeholder != nil {
		w.fs.pending.put(key(w.path), w.placeholder, w.fs, w.fs.delayFor(w.placeholder))
	}
}

// ---------------------------------------------------------------- 路径锁

// keyedMutex 按路径串行化写操作，多个路径按固定顺序加锁避免死锁。
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(keys ...string) func() {
	uniq := map[string]bool{}
	var list []string
	for _, x := range keys {
		if !uniq[x] {
			uniq[x] = true
			list = append(list, x)
		}
	}
	sort.Strings(list)
	locks := make([]*keyLock, len(list))
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyLock{}
	}
	for i, x := range list {
		l := k.m[x]
		if l == nil {
			l = &keyLock{}
			k.m[x] = l
		}
		l.refs++
		locks[i] = l
	}
	k.mu.Unlock()
	for _, l := range locks {
		l.mu.Lock()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			for i := len(locks) - 1; i >= 0; i-- {
				locks[i].mu.Unlock()
			}
			k.mu.Lock()
			for i, x := range list {
				locks[i].refs--
				if locks[i].refs == 0 {
					delete(k.m, x)
				}
			}
			k.mu.Unlock()
		})
	}
}
