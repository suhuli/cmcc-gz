package davfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mcloudmount/internal/vfs"
)

// ---------------------------------------------------------------- 待上传文件
//
// 写入完成的文件先进入本地队列（placeholder），立即对客户端可见（目录列表、读取都由本地副本提供），
// 再由后台任务上传到云端。上传失败会按退避时间自动重试；队列持久化到磁盘，程序重启后继续上传。
//
// 0 字节的新文件也以 placeholder 表示，但延迟较长：Windows 复制文件时先 PUT 0 字节再 PUT 内容，
// 延迟后可以省掉一次“创建 + 覆盖”。

type placeholder struct {
	path     string // 由 pendingSet.mu 保护（目录改名时会更新）
	key      string // 由 pendingSet.mu 保护
	parentID string
	name     string
	created  time.Time
	size     int64
	sum      string // SHA256（小写十六进制）
	local    string // 本地暂存文件；空表示 0 字节
	replace  bool   // 上传时替换云端同名文件；为 false 时（新建空文件）云端已有同名文件则跳过

	// 以下字段由 pendingSet.mu 保护
	attempts  int
	lastErr   string
	uploading bool
	timer     *time.Timer
}

// content 表示需要真正上传（并持久化）的条目，而不只是延迟创建的空文件。
func (p *placeholder) content() bool { return p.local != "" || p.replace }

func (p *placeholder) info() *fileInfo {
	return &fileInfo{name: p.name, size: p.size, mod: p.created, etag: fmt.Sprintf(`"p-%d-%d"`, p.size, p.created.UnixNano())}
}

// clone 复制条目（不含定时器与运行状态），用于改名：避免修改仍可能被读取的旧对象。
func (p *placeholder) clone() *placeholder {
	return &placeholder{path: p.path, parentID: p.parentID, name: p.name, created: p.created,
		size: p.size, sum: p.sum, local: p.local, replace: p.replace}
}

type pendingSet struct {
	mu sync.Mutex
	m  map[string]*placeholder
}

func (s *pendingSet) get(k string) *placeholder {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[k]
}

// take 取出条目并取消定时上传。调用方须持有该路径的路径锁（上传任务上传期间也持有它，
// 因此 take 不会取到正在上传的条目）。
func (s *pendingSet) take(k string) *placeholder {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.m[k]
	if p != nil {
		delete(s.m, k)
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		p.key = ""
	}
	return p
}

// put 放入条目，delay 后开始上传/创建。
func (s *pendingSet) put(k string, p *placeholder, f *FS, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.m[k]; old != nil && old != p {
		if old.timer != nil {
			old.timer.Stop()
			old.timer = nil
		}
		old.key = ""
		if old.local != "" && old.local != p.local {
			f.removeLater(old.local)
		}
	}
	s.m[k] = p
	p.key = k
	s.armLocked(p, f, delay)
}

func (s *pendingSet) armLocked(p *placeholder, f *FS, delay time.Duration) {
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(delay, func() { f.startFlush(p) })
}

// retry 安排失败条目稍后重试。
func (s *pendingSet) retry(p *placeholder, f *FS, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.key != "" && s.m[p.key] == p {
		s.armLocked(p, f, delay)
	}
}

func (s *pendingSet) keyOf(p *placeholder) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.key != "" && s.m[p.key] == p {
		return p.key
	}
	return ""
}

// begin 确认条目仍在 k 下，标记为上传中并返回快照。
func (s *pendingSet) begin(p *placeholder, k string) (placeholder, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.key != k || s.m[k] != p {
		return placeholder{}, false
	}
	p.uploading = true
	snap := *p
	snap.timer = nil
	return snap, true
}

// finish 结束一次上传尝试。ok 时移除条目；否则记录错误并返回累计失败次数。
func (s *pendingSet) finish(p *placeholder, ok bool, err error) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.uploading = false
	if ok {
		if p.key != "" && s.m[p.key] == p {
			delete(s.m, p.key)
		}
		p.key = ""
		return 0
	}
	if err != nil {
		p.attempts++
		p.lastErr = err.Error()
	}
	return p.attempts
}

func (s *pendingSet) remove(p *placeholder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.key != "" && s.m[p.key] == p {
		delete(s.m, p.key)
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
	}
	p.key = ""
}

// inDir 返回某目录下的条目快照。
func (s *pendingSet) inDir(parentID string) []placeholder {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []placeholder
	for _, p := range s.m {
		if p.parentID == parentID {
			c := *p
			c.timer = nil
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// snapshot 返回所有条目的快照。
func (s *pendingSet) snapshot() []placeholder {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]placeholder, 0, len(s.m))
	for _, p := range s.m {
		c := *p
		c.timer = nil
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// stopTimers 停止所有定时器（关闭时使用），返回所有条目。
func (s *pendingSet) stopTimers() []*placeholder {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*placeholder, 0, len(s.m))
	for _, p := range s.m {
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// rekeyPrefix 目录改名/移动后更新其下条目的路径（条目的 parentID 不变）。
func (s *pendingSet) rekeyPrefix(oldPath, newPath string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := key(oldPath) + "/"
	changed := false
	for k, p := range s.m {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		delete(s.m, k)
		p.path = newPath + p.path[len(oldPath):]
		p.key = key(p.path)
		if old := s.m[p.key]; old != nil {
			if old.timer != nil {
				old.timer.Stop()
				old.timer = nil
			}
			old.key = ""
		}
		s.m[p.key] = p
		changed = true
	}
	return changed
}

// dropPrefix 删除目录时移除其下尚未开始上传的条目。
func (s *pendingSet) dropPrefix(dir string) []*placeholder {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := key(dir) + "/"
	var out []*placeholder
	for k, p := range s.m {
		if strings.HasPrefix(k, prefix) && !p.uploading {
			delete(s.m, k)
			if p.timer != nil {
				p.timer.Stop()
				p.timer = nil
			}
			p.key = ""
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------- 后台上传

func (f *FS) delayFor(p *placeholder) time.Duration {
	if p.content() {
		return f.UploadDelay
	}
	return f.EmptyFileDelay
}

func (f *FS) startFlush(p *placeholder) {
	f.bgMu.Lock()
	if f.closed {
		f.bgMu.Unlock()
		return
	}
	f.wg.Add(1)
	f.bgMu.Unlock()
	defer f.wg.Done()
	f.flush(p)
}

// flush 上传（或创建）一个条目。上传期间持有该路径的路径锁：同一路径的后续写入/改名/删除会等待。
func (f *FS) flush(p *placeholder) {
	if p.content() {
		select {
		case f.sem <- struct{}{}:
			defer func() { <-f.sem }()
		case <-f.ctx.Done():
			return
		}
	}
	var snap placeholder
	for {
		k := f.pending.keyOf(p)
		if k == "" {
			return // 已被取走（覆盖、改名或删除）
		}
		unlock := f.locks.lock(k)
		s, ok := f.pending.begin(p, k)
		if ok {
			snap = s
			defer unlock()
			break
		}
		unlock() // 等锁期间被改名：按新路径重试
	}
	if f.ctx.Err() != nil {
		f.pending.finish(p, false, nil)
		return
	}
	ctx, cancel := context.WithTimeout(f.ctx, 6*time.Hour)
	err := f.uploadEntry(ctx, &snap)
	cancel()
	if err == nil {
		f.pending.finish(p, true, nil)
		if snap.content() {
			f.persist()
			if snap.local != "" {
				f.removeLater(snap.local)
			}
		}
		f.changed("write", snap.path)
		return
	}
	if f.ctx.Err() != nil {
		f.pending.finish(p, false, nil) // 正在关闭：保留在队列中，下次启动继续
		return
	}
	attempts := f.pending.finish(p, false, err)
	if !snap.content() {
		slog.Error("创建空文件失败", "path", snap.path, "error", err)
		f.pending.remove(p)
		return
	}
	if attempts > len(f.RetryDelays) {
		f.pending.remove(p)
		f.persist()
		f.giveUp(&snap, err)
		return
	}
	delay := f.RetryDelays[attempts-1]
	slog.Warn("上传失败，稍后自动重试", "path", snap.path, "attempt", attempts, "retry_in", delay.String(), "error", err)
	f.pending.retry(p, f, delay)
	f.persist()
}

// uploadEntry 把条目上传到云端，必要时替换同名文件。
func (f *FS) uploadEntry(ctx context.Context, p *placeholder) error {
	existing, ok, err := f.v.Lookup(ctx, p.parentID, p.name)
	if err != nil {
		return err
	}
	var ex *vfs.Entry
	if ok {
		if existing.IsDir {
			return fmt.Errorf("已存在同名文件夹 %q", existing.Name)
		}
		if !p.replace {
			return nil // 新建空文件，但云端已有同名文件（例如被其他客户端创建）
		}
		if existing.Size == p.size && existing.Hash != "" && strings.EqualFold(existing.Hash, p.sum) {
			slog.Info("内容未变化，跳过上传", "path", p.path)
			return nil
		}
		ex = &existing
	}
	var src io.ReaderAt = strings.NewReader("")
	if p.local != "" {
		fh, err := os.Open(p.local)
		if err != nil {
			return fmt.Errorf("读取本地暂存文件失败: %w", err)
		}
		defer fh.Close()
		src = fh
	}
	return f.commitUpload(ctx, p.parentID, p.name, p.path, ex, src, p.size, p.sum)
}

// giveUp 多次重试仍失败：把本地副本移到 failed 目录保存，并记录下来供界面显示。
func (f *FS) giveUp(p *placeholder, err error) {
	saved := ""
	if p.local != "" {
		dir := filepath.Join(f.stageDir, "failed")
		if mkErr := os.MkdirAll(dir, 0o700); mkErr == nil {
			dst := filepath.Join(dir, time.Now().Format("20060102-150405")+"_"+p.name)
			if os.Rename(p.local, dst) == nil {
				saved = dst
			}
		}
		if saved == "" {
			saved = p.local
		}
	}
	slog.Error("上传多次失败，已放弃（本地副本已保留）", "path", p.path, "saved", saved, "error", err)
	f.statMu.Lock()
	f.failed = append(f.failed, FailedUpload{Path: p.path, Error: err.Error(), Saved: saved, Time: time.Now().Unix()})
	if len(f.failed) > 20 {
		f.failed = f.failed[len(f.failed)-20:]
	}
	f.statMu.Unlock()
}

// removeLater 删除本地暂存文件。Windows 上文件仍被读取时无法删除，稍后重试。
func (f *FS) removeLater(name string) {
	if err := os.Remove(name); err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}
	go func() {
		for i := 0; i < 40; i++ {
			time.Sleep(15 * time.Second)
			if err := os.Remove(name); err == nil || errors.Is(err, fs.ErrNotExist) {
				return
			}
		}
	}()
}

// ---------------------------------------------------------------- 状态

// UploadItem 是队列中的一个文件。
type UploadItem struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Uploading bool   `json:"uploading"`
	Attempts  int    `json:"attempts"`
	Error     string `json:"error,omitempty"`
}

// FailedUpload 是放弃上传的文件。
type FailedUpload struct {
	Path  string `json:"path"`
	Error string `json:"error"`
	Saved string `json:"saved"` // 本地副本位置
	Time  int64  `json:"time"`
}

// UploadStats 是后台上传队列的状态。
type UploadStats struct {
	Pending int            `json:"pending"`
	Items   []UploadItem   `json:"items"`
	Failed  []FailedUpload `json:"failed"`
}

// UploadStats 返回后台上传队列的状态（不含延迟创建的空文件）。
func (f *FS) UploadStats() UploadStats {
	st := UploadStats{Items: []UploadItem{}, Failed: []FailedUpload{}}
	for _, p := range f.pending.snapshot() {
		if !p.content() {
			continue
		}
		st.Items = append(st.Items, UploadItem{Path: p.path, Size: p.size, Uploading: p.uploading, Attempts: p.attempts, Error: p.lastErr})
	}
	st.Pending = len(st.Items)
	f.statMu.Lock()
	st.Failed = append(st.Failed, f.failed...)
	f.statMu.Unlock()
	return st
}

// WaitUploads 等待后台上传队列清空（不含延迟创建的空文件）。
func (f *FS) WaitUploads(ctx context.Context) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		busy := false
		for _, p := range f.pending.snapshot() {
			if p.content() {
				busy = true
				break
			}
		}
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------- 持久化

const queueFileName = "queue.json"

type queueRecord struct {
	Path     string    `json:"path"`
	ParentID string    `json:"parent_id"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	Local    string    `json:"local,omitempty"` // stageDir 下的文件名
	Replace  bool      `json:"replace"`
	Created  time.Time `json:"created"`
	Attempts int       `json:"attempts,omitempty"`
}

// persist 把队列写入磁盘（原子替换）。未启用持久化时不做任何事。
func (f *FS) persist() {
	if !f.persistent {
		return
	}
	f.persistMu.Lock()
	defer f.persistMu.Unlock()
	recs := []queueRecord{}
	for _, p := range f.pending.snapshot() {
		if !p.content() {
			continue
		}
		r := queueRecord{Path: p.path, ParentID: p.parentID, Name: p.name, Size: p.size, SHA256: p.sum, Replace: p.replace, Created: p.created, Attempts: p.attempts}
		if p.local != "" {
			r.Local = filepath.Base(p.local)
		}
		recs = append(recs, r)
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return
	}
	file := filepath.Join(f.stageDir, queueFileName)
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("保存上传队列失败", "error", err)
		return
	}
	if err := os.Rename(tmp, file); err != nil {
		slog.Warn("保存上传队列失败", "error", err)
	}
}

// loadQueue 读取上次未完成的上传队列，并清理不再需要的暂存文件。
func (f *FS) loadQueue() {
	keep := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(f.stageDir, queueFileName))
	var recs []queueRecord
	if err == nil {
		if err := json.Unmarshal(data, &recs); err != nil {
			slog.Warn("上传队列文件损坏，已忽略", "error", err)
			recs = nil
		}
	}
	resumed := 0
	for _, r := range recs {
		p := &placeholder{path: clean(r.Path), parentID: r.ParentID, name: r.Name, created: r.Created, size: r.Size, sum: r.SHA256, replace: r.Replace, attempts: r.Attempts}
		if r.Local != "" {
			local := filepath.Join(f.stageDir, filepath.Base(r.Local))
			fi, err := os.Stat(local)
			if err != nil || fi.Size() != r.Size {
				slog.Warn("上传队列中的本地文件丢失，已跳过", "path", r.Path)
				continue
			}
			p.local = local
			keep[filepath.Base(local)] = true
		}
		if p.parentID == "" || p.name == "" {
			continue
		}
		f.pending.put(key(p.path), p, f, resumeDelay)
		resumed++
	}
	if ents, err := os.ReadDir(f.stageDir); err == nil {
		for _, e := range ents {
			n := e.Name()
			if e.IsDir() || n == queueFileName || keep[n] {
				continue
			}
			_ = os.Remove(filepath.Join(f.stageDir, n))
		}
	}
	if resumed > 0 {
		slog.Info("继续上次未完成的上传", "count", resumed)
	}
	f.persist()
}

const resumeDelay = 3 * time.Second

// ---------------------------------------------------------------- 读取待上传的文件

// stagedFile 是待上传文件的只读句柄，内容来自本地暂存文件。
type stagedFile struct {
	fs   *FS
	path string
	fi   fs.FileInfo
	f    *os.File
}

func (s *stagedFile) Read(p []byte) (int, error)                   { return s.f.Read(p) }
func (s *stagedFile) Seek(offset int64, whence int) (int64, error) { return s.f.Seek(offset, whence) }
func (s *stagedFile) Close() error                                 { return s.f.Close() }
func (s *stagedFile) Readdir(int) ([]fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "readdir", Path: s.path, Err: ErrNotDir}
}
func (s *stagedFile) Stat() (fs.FileInfo, error) { return s.fi, nil }
func (s *stagedFile) Write([]byte) (int, error)  { return 0, fs.ErrPermission }
func (s *stagedFile) DeadProps() (map[xmlName]Property, error) {
	return s.fs.props.get(key(s.path)), nil
}
func (s *stagedFile) Patch(p []Proppatch) ([]Propstat, error) {
	return s.fs.props.patch(key(s.path), p), nil
}
