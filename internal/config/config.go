// Package config 负责本地配置与登录态的读写。
//
// 配置文件默认位于 %APPDATA%\mCloudMount\config.json（可用 MCLOUDMOUNT_HOME 覆盖），
// 格式与旧版 Python 实现兼容：旧版登录态可以直接沿用，未知字段在写回时原样保留。
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 旧版本使用的固定 WebDAV 密码。检测到后会自动替换为随机密码。
const legacyDavPassword = "mcloud"

// Account 是登录后保存的账号信息。字段名与旧版 config.json 保持一致。
type Account struct {
	Phone         string         `json:"phone"`
	Token         string         `json:"token"`
	TokenExpireMs int64          `json:"token_expire_ms"`
	RefreshToken  string         `json:"refresh_token"`
	UserID        string         `json:"user_id"`
	DeviceID      string         `json:"device_id"`
	LoginID       string         `json:"login_id"`
	Account       string         `json:"account"`
	ServerInfo    map[string]any `json:"serverinfo"`
	ExtInfo       map[string]any `json:"ext_info"`
}

// LoggedIn 表示本机是否保存了可用的登录态（不检查过期）。
func (a Account) LoggedIn() bool { return a.Phone != "" && a.Token != "" }

// Expired 表示令牌是否已经过期。
func (a Account) Expired(now time.Time) bool {
	return a.TokenExpireMs > 0 && a.TokenExpireMs <= now.UnixMilli()
}

// Ext 读取 ext_info 中的字符串字段。
func (a Account) Ext(key string) string {
	if v, ok := a.ExtInfo[key].(string); ok {
		return v
	}
	return ""
}

// Clone 深拷贝账号（map 字段单独复制，避免并发修改）。
func (a Account) Clone() Account {
	b := a
	b.ServerInfo = cloneMap(a.ServerInfo)
	b.ExtInfo = cloneMap(a.ExtInfo)
	return b
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// MountSettings 是挂载参数。
type MountSettings struct {
	Drive       string `json:"drive"`
	Port        int    `json:"port"`
	Host        string `json:"host"`
	DavUser     string `json:"dav_user"`
	DavPassword string `json:"dav_password"`
}

// Config 是完整配置。
type Config struct {
	Account   Account       `json:"account"`
	Mount     MountSettings `json:"mount"`
	APIHost   string        `json:"api_host"`
	Env       string        `json:"env"`
	VerifySSL bool          `json:"verify_ssl"`
	LogLevel  string        `json:"log_level"`
	AutoMount bool          `json:"auto_mount"`
	PanelPort int           `json:"panel_port"`

	// extra 保存本版本不认识的顶层字段，写回时原样输出。
	extra map[string]json.RawMessage
}

// Default 返回默认配置。
func Default() Config {
	return Config{
		Account: Account{ServerInfo: map[string]any{}, ExtInfo: map[string]any{}},
		Mount: MountSettings{
			Drive:   "Z:",
			Port:    8380,
			Host:    "127.0.0.1",
			DavUser: "mcloud",
		},
		Env:       "prod",
		VerifySSL: true,
		LogLevel:  "INFO",
		PanelPort: 8390,
	}
}

// Clone 深拷贝配置。
func (c Config) Clone() Config {
	d := c
	d.Account = c.Account.Clone()
	if c.extra != nil {
		d.extra = make(map[string]json.RawMessage, len(c.extra))
		for k, v := range c.extra {
			d.extra[k] = append(json.RawMessage(nil), v...)
		}
	}
	return d
}

var knownKeys = map[string]bool{
	"account": true, "mount": true, "api_host": true, "env": true, "verify_ssl": true,
	"log_level": true, "auto_mount": true, "panel_port": true,
}

// UnmarshalJSON 在默认值基础上解析，并保留未知字段。
func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	p := plain(Default())
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	*c = Config(p)
	for k, v := range all {
		if !knownKeys[k] {
			if c.extra == nil {
				c.extra = map[string]json.RawMessage{}
			}
			c.extra[k] = v
		}
	}
	return nil
}

// MarshalJSON 输出已知字段与保留的未知字段。
func (c Config) MarshalJSON() ([]byte, error) {
	type plain Config
	known, err := json.Marshal(plain(c))
	if err != nil {
		return nil, err
	}
	if len(c.extra) == 0 {
		return known, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(known, &merged); err != nil {
		return nil, err
	}
	for k, v := range c.extra {
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}

// normalize 修正缺失或非法的字段。返回值表示是否有改动需要写回。
func (c *Config) normalize() bool {
	changed := false
	d := Default()
	if c.Account.ServerInfo == nil {
		c.Account.ServerInfo = map[string]any{}
	}
	if c.Account.ExtInfo == nil {
		c.Account.ExtInfo = map[string]any{}
	}
	drive := strings.ToUpper(strings.TrimSpace(c.Mount.Drive))
	if len(drive) == 1 {
		drive += ":"
	}
	if drive != c.Mount.Drive {
		c.Mount.Drive = drive
		changed = true
	}
	if c.Mount.Port <= 0 || c.Mount.Port > 65535 {
		c.Mount.Port = d.Mount.Port
		changed = true
	}
	if c.Mount.Host == "" {
		c.Mount.Host = d.Mount.Host
		changed = true
	}
	if c.Mount.DavUser == "" {
		c.Mount.DavUser = d.Mount.DavUser
		changed = true
	}
	if c.Mount.DavPassword == "" || c.Mount.DavPassword == legacyDavPassword {
		c.Mount.DavPassword = RandomString(24)
		changed = true
	}
	if c.PanelPort <= 0 || c.PanelPort > 65535 {
		c.PanelPort = d.PanelPort
		changed = true
	}
	if c.LogLevel == "" {
		c.LogLevel = d.LogLevel
	}
	if c.Env == "" {
		c.Env = d.Env
	}
	return changed
}

// ValidDrive 校验盘符格式（D: 到 Z:，空字符串表示不挂载盘符只启动 WebDAV）。
func ValidDrive(drive string) bool {
	if drive == "" {
		return true
	}
	if len(drive) != 2 || drive[1] != ':' {
		return false
	}
	return drive[0] >= 'D' && drive[0] <= 'Z'
}

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// RandomString 生成密码学安全的随机字母数字串。
func RandomString(n int) string {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alnum)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(fmt.Sprintf("crypto/rand 不可用: %v", err))
		}
		out[i] = alnum[v.Int64()]
	}
	return string(out)
}

// Dir 返回配置目录：优先 MCLOUDMOUNT_HOME，否则 %APPDATA%\mCloudMount。
func Dir() string {
	if base := os.Getenv("MCLOUDMOUNT_HOME"); base != "" {
		return base
	}
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		return filepath.Join(appdata, "mCloudMount")
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "mCloudMount")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mCloudMount")
}

// DefaultPath 返回默认配置文件路径。
func DefaultPath() string { return filepath.Join(Dir(), "config.json") }

// Store 是线程安全的配置存储。所有读取返回副本，修改通过 Update 原子完成并落盘。
type Store struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

// Open 读取配置文件。文件不存在时使用默认值；文件损坏时备份为 .bad 并使用默认值。
func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath()
	}
	s := &Store{path: path, cfg: Default()}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.cfg.normalize()
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &cfg); err != nil {
		backup := path + ".bad"
		if rerr := os.Rename(path, backup); rerr != nil {
			backup = path
		}
		slog.Warn("配置文件损坏，已备份并重置，请重新登录", "error", err, "backup", backup)
		s.cfg.normalize()
		return s, nil
	}
	s.cfg = cfg
	if s.cfg.normalize() {
		if err := s.saveLocked(); err != nil {
			slog.Warn("配置补全后写回失败", "error", err)
		}
	}
	return s, nil
}

// Path 返回配置文件路径。
func (s *Store) Path() string { return s.path }

// Get 返回配置副本。
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// Account 返回账号副本。
func (s *Store) Account() Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Account.Clone()
}

// Update 在锁内修改配置并写盘。fn 返回错误时不做任何修改。
func (s *Store) Update(fn func(c *Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	next.normalize()
	prev := s.cfg
	s.cfg = next
	if err := s.saveLocked(); err != nil {
		s.cfg = prev
		return err
	}
	return nil
}

// Reload 从磁盘重新读取（其他进程修改了配置时使用）。
func (s *Store) Reload() error {
	fresh, err := Open(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = fresh.Get()
	s.mu.Unlock()
	return nil
}

func (s *Store) saveLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入配置失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("写入配置失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	if err := replaceFile(tmpName, s.path); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	restrictToOwner(s.path)
	return nil
}
