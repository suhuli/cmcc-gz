// Package davfs 把云盘实现为 golang.org/x/net/webdav 的 FileSystem。
package davfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"mcloudmount/internal/cloud"
	"mcloudmount/internal/config"
	"mcloudmount/internal/vfs"
)

// Cloud 是 davfs 依赖的云端能力（由 *cloud.Client 实现）。
type Cloud interface {
	vfs.Lister
	CreateFolder(ctx context.Context, parentID, name string) (cloud.FileItem, error)
	Rename(ctx context.Context, fileID, newName string) error
	Move(ctx context.Context, fileIDs []string, toParentID string) error
	Trash(ctx context.Context, fileIDs []string) error
	OpenFile(ctx context.Context, fileID string, offset int64) (io.ReadCloser, error)
	Upload(ctx context.Context, r cloud.UploadRequest) (cloud.UploadResult, error)
}

// 内部临时文件名前缀：替换文件时使用，不在目录列表中显示。
const (
	tmpPrefix    = "__mcm_tmp_"
	backupPrefix = "__mcm_bak_"
)

func internalName(name string) bool {
	return strings.HasPrefix(name, tmpPrefix) || strings.HasPrefix(name, backupPrefix)
}

var emptySHA256 = func() string { s := sha256.Sum256(nil); return hex.EncodeToString(s[:]) }()

// FS 实现 webdav.FileSystem。
type FS struct {
	c      Cloud
	v      *vfs.FS
	tmpDir string

	// EmptyFileDelay 是 0 字节新文件延迟创建的时间：
	// Windows 复制文件时先 PUT 0 字节再 PUT 内容，延迟后可以省掉一次“创建 + 覆盖”。
	EmptyFileDelay time.Duration
	// ConsistencyWait 是改名/移动后等待服务端一致的最长时间。
	ConsistencyWait time.Duration
	// OnChange 在内容变化后回调（可选，用于日志或通知）。
	OnChange func(op, path string)

	props   *propStore
	pending *pendingSet
	locks   keyedMutex

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 创建文件系统。tmpDir 用于暂存上传内容，为空时使用系统临时目录。
func New(c Cloud, v *vfs.FS, tmpDir string) (*FS, error) {
	if tmpDir == "" {
		tmpDir = filepath.Join(os.TempDir(), "mcloudmount-upload")
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建上传缓存目录失败: %w", err)
	}
	cleanTmp(tmpDir)
	ctx, cancel := context.WithCancel(context.Background())
	f := &FS{
		c:               c,
		v:               v,
		tmpDir:          tmpDir,
		EmptyFileDelay:  5 * time.Second,
		ConsistencyWait: 10 * time.Second,
		props:           newPropStore(),
		ctx:             ctx,
		cancel:          cancel,
	}
	f.pending = &pendingSet{m: map[string]*placeholder{}}
	return f, nil
}

// cleanTmp 删除上次异常退出遗留的上传缓存。
func cleanTmp(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "put-") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// VFS 返回底层路径缓存。
func (f *FS) VFS() *vfs.FS { return f.v }

// Close 立即创建所有待创建的空文件，并停止后台任务。
func (f *FS) Close() error {
	for _, p := range f.pending.drain() {
		f.flushPlaceholder(p)
	}
	f.cancel()
	f.wg.Wait()
	return nil
}

func clean(name string) string { return path.Clean("/" + name) }

func key(name string) string { return strings.ToLower(clean(name)) }

func (f *FS) changed(op, p string) {
	if f.OnChange != nil {
		f.OnChange(op, p)
	}
}

// mapErr 把云端错误转换为 webdav 能识别的错误。
func mapErr(op, name string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist), cloud.IsKind(err, cloud.KindNotFound):
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	case errors.Is(err, fs.ErrExist), cloud.IsKind(err, cloud.KindConflict):
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrExist}
	case cloud.IsKind(err, cloud.KindAuth):
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrPermission}
	}
	return &fs.PathError{Op: op, Path: name, Err: err}
}

// ---------------------------------------------------------------- FileSystem

// Stat 返回文件信息。
func (f *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	name = clean(name)
	if name == "/" {
		return dirInfo(vfs.Root()), nil
	}
	if p := f.pending.get(key(name)); p != nil {
		return p.info(), nil
	}
	e, err := f.v.Resolve(ctx, name)
	if err != nil {
		return nil, mapErr("stat", name, err)
	}
	return entryInfo(e), nil
}

// Mkdir 创建目录。
func (f *FS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	name = clean(name)
	if name == "/" {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	unlock := f.locks.lock(key(name))
	defer unlock()
	parent, base, err := f.v.ResolveParent(ctx, name)
	if err != nil {
		return mapErr("mkdir", name, err)
	}
	if err := validName(base); err != nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: err}
	}
	if _, ok, err := f.v.Lookup(ctx, parent.ID, base); err != nil {
		return mapErr("mkdir", name, err)
	} else if ok || f.pending.get(key(name)) != nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	item, err := f.c.CreateFolder(ctx, parent.ID, base)
	if err != nil {
		return mapErr("mkdir", name, err)
	}
	if item.ID == "" {
		f.v.Invalidate(parent.ID)
	} else {
		f.v.Added(vfs.Entry{ID: item.ID, ParentID: parent.ID, Name: item.Name, IsDir: true, Mod: item.Updated, Created: item.Created})
	}
	slog.Info("创建目录", "path", name)
	f.changed("mkdir", name)
	return nil
}

// RemoveAll 删除文件或目录（移入云盘回收站）。
func (f *FS) RemoveAll(ctx context.Context, name string) error {
	name = clean(name)
	if name == "/" {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrPermission}
	}
	unlock := f.locks.lock(key(name))
	defer unlock()
	if p := f.pending.take(key(name)); p != nil {
		f.props.removeTree(key(name))
		return nil
	}
	e, err := f.v.Resolve(ctx, name)
	if err != nil {
		return mapErr("remove", name, err)
	}
	if err := f.c.Trash(ctx, []string{e.ID}); err != nil {
		return mapErr("remove", name, err)
	}
	f.v.Removed(e)
	f.props.removeTree(key(name))
	slog.Info("删除", "path", name)
	f.changed("remove", name)
	return nil
}

// Rename 改名或移动。目标已存在时由 webdav 层事先删除（Overwrite: T）。
func (f *FS) Rename(ctx context.Context, oldName, newName string) error {
	oldName, newName = clean(oldName), clean(newName)
	if oldName == "/" || newName == "/" {
		return &fs.PathError{Op: "rename", Path: oldName, Err: fs.ErrPermission}
	}
	if strings.HasPrefix(key(newName)+"/", key(oldName)+"/") && key(newName) != key(oldName) {
		return &fs.PathError{Op: "rename", Path: oldName, Err: fs.ErrInvalid}
	}
	unlock := f.locks.lock(key(oldName), key(newName))
	defer unlock()

	dstParent, dstBase, err := f.v.ResolveParent(ctx, newName)
	if err != nil {
		return mapErr("rename", newName, err)
	}
	if err := validName(dstBase); err != nil {
		return &fs.PathError{Op: "rename", Path: newName, Err: err}
	}
	// 尚未上传的空文件：只改本地记录
	if p := f.pending.take(key(oldName)); p != nil {
		p.path, p.parentID, p.name = newName, dstParent.ID, dstBase
		f.pending.put(key(newName), p, f)
		f.props.move(key(oldName), key(newName))
		return nil
	}
	src, err := f.v.Resolve(ctx, oldName)
	if err != nil {
		return mapErr("rename", oldName, err)
	}
	if err := f.moveEntry(ctx, src, dstParent, dstBase); err != nil {
		return mapErr("rename", oldName, err)
	}
	f.props.move(key(oldName), key(newName))
	slog.Info("移动/改名", "from", oldName, "to", newName)
	f.changed("rename", newName)
	return nil
}

// moveEntry 在云端执行改名/移动，并更新缓存。
func (f *FS) moveEntry(ctx context.Context, src vfs.Entry, dstParent vfs.Entry, dstName string) error {
	if src.ParentID == dstParent.ID {
		if src.Name == dstName {
			return nil
		}
		if err := f.retryConsistency(ctx, func() error { return f.c.Rename(ctx, src.ID, dstName) }); err != nil {
			return err
		}
		f.v.Moved(src, dstParent.ID, dstName)
		return nil
	}
	cur := src
	// 目标目录里已有与源同名的其他条目时，先改成临时名再移动，避免冲突
	if other, ok, err := f.v.Lookup(ctx, dstParent.ID, src.Name); err != nil {
		return err
	} else if ok && other.ID != src.ID && src.Name != dstName {
		tmp := tmpPrefix + config.RandomString(8) + "_" + src.Name
		if err := f.c.Rename(ctx, src.ID, tmp); err != nil {
			return err
		}
		cur = f.v.Moved(cur, cur.ParentID, tmp)
	}
	if err := f.c.Move(ctx, []string{cur.ID}, dstParent.ID); err != nil {
		if cur.Name != src.Name {
			_ = f.c.Rename(context.WithoutCancel(ctx), cur.ID, src.Name)
			f.v.Moved(cur, cur.ParentID, src.Name)
		}
		return err
	}
	cur = f.v.Moved(cur, dstParent.ID, cur.Name)
	if cur.Name != dstName {
		if err := f.retryConsistency(ctx, func() error { return f.c.Rename(ctx, cur.ID, dstName) }); err != nil {
			return err
		}
		f.v.Moved(cur, dstParent.ID, dstName)
	}
	return nil
}

// retryConsistency 重试因服务端最终一致性导致的冲突/找不到。
func (f *FS) retryConsistency(ctx context.Context, op func() error) error {
	deadline := time.Now().Add(f.ConsistencyWait)
	delay := 200 * time.Millisecond
	for {
		err := op()
		if err == nil {
			return nil
		}
		if !(cloud.IsKind(err, cloud.KindConflict) || cloud.IsKind(err, cloud.KindNotFound)) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// OpenFile 打开文件或目录。写入模式返回暂存到本地临时文件的句柄，Close 时上传。
func (f *FS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	name = clean(name)
	// 只有创建/截断/只写/追加才是真正的写入。单独的 O_RDWR 来自 PROPPATCH
	// （webdav 用它取得属性句柄，不会写入内容）：若按写入处理，关闭时会用 0 字节覆盖文件。
	if flag&(os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return f.openWrite(ctx, name, flag)
	}
	if name == "/" {
		return &dirFile{fs: f, e: vfs.Root(), path: name, ctx: ctx}, nil
	}
	if p := f.pending.get(key(name)); p != nil {
		return &emptyFile{fs: f, path: name, fi: p.info()}, nil
	}
	e, err := f.v.Resolve(ctx, name)
	if err != nil {
		return nil, mapErr("open", name, err)
	}
	if e.IsDir {
		return &dirFile{fs: f, e: e, path: name, ctx: ctx}, nil
	}
	return &readFile{fs: f, e: e, path: name, ctx: ctx, guard: guardFrom(ctx)}, nil
}

func (f *FS) openWrite(ctx context.Context, name string, flag int) (webdav.File, error) {
	if name == "/" {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	unlock := f.locks.lock(key(name))
	parent, base, err := f.v.ResolveParent(ctx, name)
	if err != nil {
		unlock()
		return nil, mapErr("open", name, err)
	}
	if err := validName(base); err != nil {
		unlock()
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	w := &writeFile{fs: f, path: name, parent: parent, name: base, ctx: ctx, guard: guardFrom(ctx), unlock: unlock, mod: time.Now(),
		truncate: flag&os.O_TRUNC != 0}
	if p := f.pending.take(key(name)); p != nil {
		w.placeholder = p
	} else {
		existing, ok, err := f.v.Lookup(ctx, parent.ID, base)
		if err != nil {
			unlock()
			return nil, mapErr("open", name, err)
		}
		if ok {
			if existing.IsDir {
				unlock()
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrExist}
			}
			if flag&os.O_EXCL != 0 {
				unlock()
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrExist}
			}
			w.existing = &existing
			w.name = existing.Name // 不区分大小写命中时保留云端原名
		}
	}
	tmp, err := os.CreateTemp(f.tmpDir, "put-*")
	if err != nil {
		if w.placeholder != nil {
			f.pending.put(key(name), w.placeholder, f)
		}
		unlock()
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	w.tmp = tmp
	w.hasher = sha256.New()
	w.seqOK = true
	return w, nil
}

// validName 拒绝云端或 Windows 不支持的名字。
func validName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return fs.ErrInvalid
	}
	if strings.ContainsAny(name, "\\/:*?\"<>|\x00") {
		return fs.ErrInvalid
	}
	return nil
}

// ---------------------------------------------------------------- 上传与替换

// commitUpload 把暂存内容上传为 parent/name；existing 非空时安全替换旧文件。
func (f *FS) commitUpload(ctx context.Context, parent vfs.Entry, name string, existing *vfs.Entry, src io.ReaderAt, size int64, sum string) error {
	start := time.Now()
	uploadName := name
	if existing != nil {
		uploadName = tmpPrefix + config.RandomString(8) + "_" + name
	}
	res, err := f.c.Upload(ctx, cloud.UploadRequest{ParentID: parent.ID, Name: uploadName, Size: size, SHA256: sum, Source: src})
	if err != nil {
		return err
	}
	newEntry := vfs.Entry{ID: res.FileID, ParentID: parent.ID, Name: res.Name, Size: size, Mod: time.Now(), Created: time.Now(), Hash: sum}
	if existing == nil && res.Name != name {
		// 缓存里没有、云端却已有同名文件（被自动改名）：按替换处理
		f.v.Invalidate(parent.ID)
		old, ok, lerr := f.v.Lookup(ctx, parent.ID, name)
		if lerr == nil && ok && !old.IsDir && old.ID != res.FileID {
			existing = &old
		} else {
			f.v.Added(newEntry)
			slog.Warn("同名文件已存在，云端自动改名", "want", name, "got", res.Name)
			return nil
		}
	}
	if existing != nil {
		if err := f.swapIn(ctx, parent, name, newEntry, *existing); err != nil {
			return err
		}
	} else {
		f.v.Added(newEntry)
	}
	mode := "上传"
	if res.Rapid {
		mode = "秒传"
	}
	slog.Info(mode+"完成", "path", path.Join("/", parent.Name, name), "size", size, "elapsed", time.Since(start).Round(time.Millisecond))
	return nil
}

// swapIn 用 newEntry 替换 old：旧文件改为备份名 -> 新文件改为目标名 -> 删除备份。
// 任一步失败都会尽量还原旧文件并清理新上传的临时文件。
func (f *FS) swapIn(ctx context.Context, parent vfs.Entry, name string, newEntry, old vfs.Entry) error {
	bg := context.WithoutCancel(ctx)
	backup := backupPrefix + config.RandomString(8) + "_" + name
	if err := f.c.Rename(ctx, old.ID, backup); err != nil {
		f.discard(bg, newEntry)
		return err
	}
	err := f.retryConsistency(bg, func() error { return f.c.Rename(bg, newEntry.ID, name) })
	if err != nil {
		if rerr := f.c.Rename(bg, old.ID, old.Name); rerr != nil {
			slog.Error("替换失败且无法还原旧文件，旧内容保留为备份", "backup", backup, "error", rerr)
		}
		f.discard(bg, newEntry)
		f.v.Invalidate(parent.ID)
		return err
	}
	if terr := f.c.Trash(bg, []string{old.ID}); terr != nil {
		slog.Warn("新文件已生效，但旧文件备份未能移入回收站", "backup", backup, "error", terr)
	}
	f.v.Removed(old)
	newEntry.Name = name
	f.v.Added(newEntry)
	return nil
}

func (f *FS) discard(ctx context.Context, e vfs.Entry) {
	if err := f.c.Trash(ctx, []string{e.ID}); err != nil {
		slog.Warn("无法清理失败上传的临时文件", "name", e.Name, "error", err)
	}
}

// ---------------------------------------------------------------- 文件信息

type fileInfo struct {
	name string
	size int64
	mod  time.Time
	dir  bool
	etag string
}

var epoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func entryInfo(e vfs.Entry) *fileInfo {
	mod := e.Mod
	if mod.IsZero() {
		mod = e.Created
	}
	if mod.IsZero() {
		mod = epoch
	}
	tag := e.Hash
	if tag == "" {
		tag = fmt.Sprintf("%s-%d-%d", e.ID, e.Size, mod.Unix())
	}
	return &fileInfo{name: e.Name, size: e.Size, mod: mod, dir: e.IsDir, etag: `"` + tag + `"`}
}

func dirInfo(e vfs.Entry) *fileInfo {
	fi := entryInfo(e)
	fi.dir = true
	if fi.name == "" {
		fi.name = "/"
	}
	return fi
}

func (i *fileInfo) Name() string       { return i.name }
func (i *fileInfo) Size() int64        { return i.size }
func (i *fileInfo) ModTime() time.Time { return i.mod }
func (i *fileInfo) IsDir() bool        { return i.dir }
func (i *fileInfo) Sys() any           { return nil }
func (i *fileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

// ContentType 根据扩展名判断类型，避免 webdav 为了嗅探类型而下载文件内容。
func (i *fileInfo) ContentType(context.Context) (string, error) {
	if i.dir {
		return "httpd/unix-directory", nil
	}
	if t := mime.TypeByExtension(path.Ext(i.name)); t != "" {
		return t, nil
	}
	return "application/octet-stream", nil
}

// ETag 使用内容哈希（或 ID + 大小 + 时间）。
func (i *fileInfo) ETag(context.Context) (string, error) { return i.etag, nil }

// ---------------------------------------------------------------- 目录句柄

type dirFile struct {
	fs      *FS
	e       vfs.Entry
	path    string
	ctx     context.Context
	entries []fs.FileInfo
	loaded  bool
	pos     int
}

func (d *dirFile) load() error {
	if d.loaded {
		return nil
	}
	list, err := d.fs.v.List(d.ctx, d.e.ID)
	if err != nil {
		return mapErr("readdir", d.path, err)
	}
	seen := map[string]bool{}
	for _, e := range list {
		if internalName(e.Name) {
			continue
		}
		seen[strings.ToLower(e.Name)] = true
		d.entries = append(d.entries, entryInfo(e))
	}
	for _, p := range d.fs.pending.inDir(d.e.ID) {
		if !seen[strings.ToLower(p.name)] {
			d.entries = append(d.entries, p.info())
		}
	}
	sort.Slice(d.entries, func(i, j int) bool { return d.entries[i].Name() < d.entries[j].Name() })
	d.loaded = true
	return nil
}

func (d *dirFile) Readdir(count int) ([]fs.FileInfo, error) {
	if err := d.load(); err != nil {
		return nil, err
	}
	rest := d.entries[d.pos:]
	if count <= 0 {
		d.pos = len(d.entries)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if count > len(rest) {
		count = len(rest)
	}
	d.pos += count
	return rest[:count], nil
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return dirInfo(d.e), nil }
func (d *dirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.path, Err: errIsDir}
}
func (d *dirFile) Write([]byte) (int, error) {
	return 0, &fs.PathError{Op: "write", Path: d.path, Err: errIsDir}
}
func (d *dirFile) Seek(int64, int) (int64, error)           { return 0, nil }
func (d *dirFile) Close() error                             { return nil }
func (d *dirFile) DeadProps() (map[xmlName]Property, error) { return d.fs.props.get(key(d.path)), nil }
func (d *dirFile) Patch(p []Proppatch) ([]Propstat, error) {
	return d.fs.props.patch(key(d.path), p), nil
}

var errIsDir = errors.New("是目录")

// ---------------------------------------------------------------- 读取句柄

type readFile struct {
	fs    *FS
	e     vfs.Entry
	path  string
	ctx   context.Context
	guard *TransferGuard

	pos    int64
	rc     io.ReadCloser
	rcPos  int64
	closed bool
}

func (r *readFile) Read(p []byte) (int, error) {
	if r.closed {
		return 0, fs.ErrClosed
	}
	if r.pos >= r.e.Size {
		return 0, io.EOF
	}
	if max := r.e.Size - r.pos; int64(len(p)) > max {
		p = p[:max]
	}
	for attempt := 0; ; attempt++ {
		if r.rc == nil || r.rcPos != r.pos {
			r.closeStream()
			rc, err := r.fs.c.OpenFile(r.ctx, r.e.ID, r.pos)
			if err != nil {
				r.guard.fail(err)
				return 0, mapErr("read", r.path, err)
			}
			r.rc, r.rcPos = rc, r.pos
		}
		n, err := r.rc.Read(p)
		r.pos += int64(n)
		r.rcPos += int64(n)
		if err == io.EOF {
			r.closeStream()
			if r.pos < r.e.Size {
				if n > 0 {
					return n, nil
				}
				if attempt < 2 {
					continue
				}
				short := fmt.Errorf("文件内容不完整：期望 %d 字节，实际 %d 字节", r.e.Size, r.pos)
				r.guard.fail(short)
				return 0, short
			}
			if n > 0 {
				return n, nil
			}
			return 0, io.EOF
		}
		if err != nil {
			r.closeStream()
			if n > 0 {
				return n, nil
			}
			if attempt < 2 && r.ctx.Err() == nil {
				slog.Debug("读取中断，从断点重连", "path", r.path, "offset", r.pos, "error", err)
				continue
			}
			r.guard.fail(err)
			return 0, err
		}
		return n, nil
	}
}

func (r *readFile) closeStream() {
	if r.rc != nil {
		_ = r.rc.Close()
		r.rc = nil
	}
}

func (r *readFile) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = r.pos + offset
	case io.SeekEnd:
		target = r.e.Size + offset
	default:
		return 0, fs.ErrInvalid
	}
	if target < 0 {
		return 0, fs.ErrInvalid
	}
	r.pos = target
	return target, nil
}

func (r *readFile) Close() error {
	r.closed = true
	r.closeStream()
	return nil
}

func (r *readFile) Readdir(int) ([]fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "readdir", Path: r.path, Err: ErrNotDir}
}
func (r *readFile) Stat() (fs.FileInfo, error) { return entryInfo(r.e), nil }
func (r *readFile) Write([]byte) (int, error) {
	return 0, &fs.PathError{Op: "write", Path: r.path, Err: fs.ErrPermission}
}
func (r *readFile) DeadProps() (map[xmlName]Property, error) { return r.fs.props.get(key(r.path)), nil }
func (r *readFile) Patch(p []Proppatch) ([]Propstat, error) {
	return r.fs.props.patch(key(r.path), p), nil
}

// ErrNotDir 表示对文件执行了目录操作。
var ErrNotDir = errors.New("不是目录")

// emptyFile 是待创建空文件的只读句柄。
type emptyFile struct {
	fs   *FS
	path string
	fi   fs.FileInfo
}

func (e *emptyFile) Read([]byte) (int, error)       { return 0, io.EOF }
func (e *emptyFile) Seek(int64, int) (int64, error) { return 0, nil }
func (e *emptyFile) Close() error                   { return nil }
func (e *emptyFile) Readdir(int) ([]fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "readdir", Path: e.path, Err: ErrNotDir}
}
func (e *emptyFile) Stat() (fs.FileInfo, error) { return e.fi, nil }
func (e *emptyFile) Write([]byte) (int, error)  { return 0, fs.ErrPermission }
func (e *emptyFile) DeadProps() (map[xmlName]Property, error) {
	return e.fs.props.get(key(e.path)), nil
}
func (e *emptyFile) Patch(p []Proppatch) ([]Propstat, error) {
	return e.fs.props.patch(key(e.path), p), nil
}
