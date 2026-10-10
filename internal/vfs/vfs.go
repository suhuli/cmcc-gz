// Package vfs 把路径解析为云端 fileId，并缓存目录内容。
//
// 设计要点：
//   - 每个目录缓存一份列表（精确名字索引 + 不区分大小写的回退索引），TTL 内不再请求云端；
//   - 同一目录的并发刷新合并为一次请求（singleflight）；
//   - 写操作成功后立即更新缓存，并登记一个短期“钉住”记录：服务端列表存在延迟时，
//     刷新结果会叠加这些记录，避免刚创建的文件消失、刚删除的文件又冒出来；
//   - 刷新失败时退回到过期缓存，保证资源管理器不会因为一次网络抖动报错。
package vfs

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"mcloudmount/internal/cloud"
)

// Entry 是一个文件或目录。
type Entry struct {
	ID       string
	ParentID string
	Name     string
	IsDir    bool
	Size     int64
	Mod      time.Time
	Created  time.Time
	Hash     string
}

// RootID 是根目录 ID。
const RootID = cloud.RootID

// Root 返回根目录条目。
func Root() Entry { return Entry{ID: RootID, Name: "", IsDir: true} }

// Lister 是 VFS 依赖的云端能力。
type Lister interface {
	ListFolder(ctx context.Context, parentID string) ([]cloud.FileItem, error)
}

const (
	DefaultTTL = 20 * time.Second
	pinTTL     = 45 * time.Second
)

// ErrNotDir 表示路径中间的某一段不是目录。
var ErrNotDir = errors.New("不是目录")

type dirCache struct {
	fetched time.Time
	byName  map[string]Entry
	byFold  map[string]string // 小写名 -> 实际名
}

func newDirCache(now time.Time) *dirCache {
	return &dirCache{fetched: now, byName: map[string]Entry{}, byFold: map[string]string{}}
}

func fold(s string) string { return strings.ToLower(s) }

func (d *dirCache) add(e Entry) {
	if old, ok := d.byName[e.Name]; ok && old.ID != e.ID {
		delete(d.byName, e.Name)
	}
	// 同一 ID 以旧名字存在时先删除（改名）
	for n, x := range d.byName {
		if x.ID == e.ID && n != e.Name {
			d.remove(n)
		}
	}
	d.byName[e.Name] = e
	if _, ok := d.byFold[fold(e.Name)]; !ok {
		d.byFold[fold(e.Name)] = e.Name
	}
}

func (d *dirCache) remove(name string) {
	if _, ok := d.byName[name]; !ok {
		return
	}
	delete(d.byName, name)
	f := fold(name)
	if d.byFold[f] == name {
		delete(d.byFold, f)
		for n := range d.byName {
			if fold(n) == f {
				d.byFold[f] = n
				break
			}
		}
	}
}

func (d *dirCache) removeID(id string) {
	for n, e := range d.byName {
		if e.ID == id {
			d.remove(n)
		}
	}
}

func (d *dirCache) lookup(name string) (Entry, bool) {
	if e, ok := d.byName[name]; ok {
		return e, true
	}
	if real, ok := d.byFold[fold(name)]; ok {
		e, ok := d.byName[real]
		return e, ok
	}
	return Entry{}, false
}

type pin struct {
	add     *Entry // 新增或改名后的条目
	removed string // 已删除/移走的 ID
	until   time.Time
}

type flight struct {
	done chan struct{}
	dir  *dirCache
	err  error
}

// FS 是带缓存的路径解析器，可并发使用。
type FS struct {
	lister Lister
	TTL    time.Duration
	Now    func() time.Time

	mu       sync.Mutex
	dirs     map[string]*dirCache
	inflight map[string]*flight
	pins     map[string][]pin
}

// New 创建 VFS。
func New(l Lister) *FS {
	return &FS{
		lister:   l,
		TTL:      DefaultTTL,
		Now:      time.Now,
		dirs:     map[string]*dirCache{},
		inflight: map[string]*flight{},
		pins:     map[string][]pin{},
	}
}

func toEntry(it cloud.FileItem, parentID string) Entry {
	return Entry{ID: it.ID, ParentID: parentID, Name: it.Name, IsDir: it.IsDir, Size: it.Size, Mod: it.Updated, Created: it.Created, Hash: it.Hash}
}

// dir 返回目录缓存（必要时刷新）。调用方不得修改返回值。
func (v *FS) dir(ctx context.Context, id string) (*dirCache, error) {
	v.mu.Lock()
	if d, ok := v.dirs[id]; ok && v.Now().Sub(d.fetched) < v.TTL {
		v.mu.Unlock()
		return d, nil
	}
	if f, ok := v.inflight[id]; ok {
		v.mu.Unlock()
		select {
		case <-f.done:
			return f.dir, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	v.inflight[id] = f
	v.mu.Unlock()

	// 刷新不受单个请求取消影响：其他等待者可能还需要结果
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	items, err := v.lister.ListFolder(fctx, id)
	cancel()

	v.mu.Lock()
	delete(v.inflight, id)
	if err != nil {
		if stale, ok := v.dirs[id]; ok && !cloud.IsKind(err, cloud.KindNotFound) && !cloud.IsKind(err, cloud.KindAuth) {
			slog.Warn("目录刷新失败，暂用缓存", "dir", id, "error", err)
			f.dir = stale
		} else {
			if cloud.IsKind(err, cloud.KindNotFound) {
				delete(v.dirs, id)
			}
			f.err = err
		}
	} else {
		d := newDirCache(v.Now())
		for _, it := range items {
			d.add(toEntry(it, id))
		}
		v.applyPinsLocked(id, d)
		v.dirs[id] = d
		f.dir = d
	}
	v.mu.Unlock()
	close(f.done)
	return f.dir, f.err
}

// applyPinsLocked 把尚未被服务端列表反映的本地修改叠加到刷新结果上。
func (v *FS) applyPinsLocked(id string, d *dirCache) {
	list := v.pins[id]
	if len(list) == 0 {
		return
	}
	now := v.Now()
	keep := list[:0]
	for _, p := range list {
		if now.After(p.until) {
			continue
		}
		switch {
		case p.removed != "":
			found := false
			for _, e := range d.byName {
				if e.ID == p.removed {
					found = true
					break
				}
			}
			if !found {
				continue // 服务端已反映
			}
			d.removeID(p.removed)
			keep = append(keep, p)
		case p.add != nil:
			if cur, ok := d.byName[p.add.Name]; ok && cur.ID == p.add.ID {
				continue // 服务端已反映
			}
			d.add(*p.add)
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		delete(v.pins, id)
	} else {
		v.pins[id] = keep
	}
}

// List 返回目录内容（按名字排序）。
func (v *FS) List(ctx context.Context, dirID string) ([]Entry, error) {
	d, err := v.dir(ctx, dirID)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	out := make([]Entry, 0, len(d.byName))
	for _, e := range d.byName {
		out = append(out, e)
	}
	v.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Lookup 在目录中查找名字：先精确匹配，再不区分大小写匹配。
func (v *FS) Lookup(ctx context.Context, dirID, name string) (Entry, bool, error) {
	d, err := v.dir(ctx, dirID)
	if err != nil {
		return Entry{}, false, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := d.lookup(name)
	return e, ok, nil
}

// Split 把路径拆成各段（忽略空段）。
func Split(p string) []string {
	p = path.Clean("/" + p)
	if p == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(p, "/"), "/")
}

// Resolve 解析路径。不存在时返回 fs.ErrNotExist。
func (v *FS) Resolve(ctx context.Context, p string) (Entry, error) {
	cur := Root()
	for _, part := range Split(p) {
		if !cur.IsDir {
			return Entry{}, fs.ErrNotExist
		}
		e, ok, err := v.Lookup(ctx, cur.ID, part)
		if err != nil {
			if cloud.IsKind(err, cloud.KindNotFound) {
				return Entry{}, fs.ErrNotExist
			}
			return Entry{}, err
		}
		if !ok {
			return Entry{}, fs.ErrNotExist
		}
		cur = e
	}
	return cur, nil
}

// ResolveParent 解析路径的父目录，返回父目录与最后一段名字。
func (v *FS) ResolveParent(ctx context.Context, p string) (Entry, string, error) {
	parts := Split(p)
	if len(parts) == 0 {
		return Entry{}, "", fs.ErrInvalid
	}
	parent, err := v.Resolve(ctx, strings.Join(parts[:len(parts)-1], "/"))
	if err != nil {
		return Entry{}, "", err
	}
	if !parent.IsDir {
		return Entry{}, "", ErrNotDir
	}
	return parent, parts[len(parts)-1], nil
}

// Added 记录新增的条目（创建、上传、移入、改名后的新名字）。
func (v *FS) Added(e Entry) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if d, ok := v.dirs[e.ParentID]; ok {
		d.add(e)
	}
	// 同一 ID 之前的删除/新增记录作废，以本次为准
	list := v.pins[e.ParentID][:0]
	for _, p := range v.pins[e.ParentID] {
		if p.removed != e.ID && (p.add == nil || p.add.ID != e.ID) {
			list = append(list, p)
		}
	}
	cp := e
	v.pins[e.ParentID] = append(list, pin{add: &cp, until: v.Now().Add(pinTTL)})
}

// Removed 记录被删除或移走的条目。
func (v *FS) Removed(e Entry) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if d, ok := v.dirs[e.ParentID]; ok {
		d.removeID(e.ID)
	}
	// 同一 ID 之前的新增记录作废
	list := v.pins[e.ParentID][:0]
	for _, p := range v.pins[e.ParentID] {
		if p.add == nil || p.add.ID != e.ID {
			list = append(list, p)
		}
	}
	v.pins[e.ParentID] = append(list, pin{removed: e.ID, until: v.Now().Add(pinTTL)})
	if e.IsDir {
		delete(v.dirs, e.ID)
	}
}

// Moved 记录改名或移动：旧位置删除，新位置新增。
func (v *FS) Moved(old Entry, newParentID, newName string) Entry {
	v.Removed(old)
	moved := old
	moved.ParentID = newParentID
	moved.Name = newName
	v.Added(moved)
	return moved
}

// Invalidate 让某个目录的缓存失效（下次访问时刷新）。
func (v *FS) Invalidate(dirID string) {
	v.mu.Lock()
	delete(v.dirs, dirID)
	v.mu.Unlock()
}

// InvalidateAll 清空全部目录缓存（保留钉住记录）。
func (v *FS) InvalidateAll() {
	v.mu.Lock()
	v.dirs = map[string]*dirCache{}
	v.mu.Unlock()
}
