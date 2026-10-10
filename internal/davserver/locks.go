package davserver

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// lenientLS 是“宽松”的锁系统：LOCK 总是成功并返回标准格式的令牌，但从不因锁拒绝请求。
//
// 为什么不用 webdav.NewMemLS：
//   - MemLS 的令牌是 "1"、"2" 这样的数字，不是 RFC 4918 要求的绝对 URI。Windows WebClient
//     加锁后发送 PUT 时没有带回可识别的令牌，结果被 423 Locked 拒绝（复制文件失败）。
//   - MemLS 同一把锁在同一时刻只能被一个请求使用，而 WebClient 会并发发送同一文件的多个请求。
//
// 本服务只供本机已认证的单个用户使用，锁不提供实际保护；同一路径的写入已由 davfs 按路径串行化。
type lenientLS struct {
	mu    sync.Mutex
	locks map[string]lockEntry // token -> 锁信息（用于 Refresh）
}

type lockEntry struct {
	details webdav.LockDetails
	expires time.Time
}

func newLenientLS() *lenientLS { return &lenientLS{locks: map[string]lockEntry{}} }

func newLockToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)
	return "opaquelocktoken:" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func lockTTL(d time.Duration) time.Duration {
	if d <= 0 || d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

func (l *lenientLS) gc(now time.Time) {
	for t, e := range l.locks {
		if now.After(e.expires) {
			delete(l.locks, t)
		}
	}
}

// Confirm 总是成功。
func (l *lenientLS) Confirm(time.Time, string, string, ...webdav.Condition) (func(), error) {
	return func() {}, nil
}

// Create 总是成功。
func (l *lenientLS) Create(now time.Time, d webdav.LockDetails) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gc(now)
	token := newLockToken()
	l.locks[token] = lockEntry{details: d, expires: now.Add(lockTTL(d.Duration))}
	return token, nil
}

// Refresh 延长锁的有效期。未知令牌（例如程序重启后）返回 ErrNoSuchLock，客户端会重新加锁。
func (l *lenientLS) Refresh(now time.Time, token string, d time.Duration) (webdav.LockDetails, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.locks[token]
	if !ok {
		return webdav.LockDetails{}, webdav.ErrNoSuchLock
	}
	e.details.Duration = d
	e.expires = now.Add(lockTTL(d))
	l.locks[token] = e
	return e.details, nil
}

// Unlock 总是成功（未知令牌也视为已解锁）。
func (l *lenientLS) Unlock(now time.Time, token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.locks, token)
	return nil
}
