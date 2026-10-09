package cloud

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Account 与 Python 版 config.json 的 account 段字段名保持一致，两个版本可共用登录态。
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

// Config 只包含 M0 需要的字段；写回时需要保留其他字段（M1 处理）。
type Config struct {
	Account   Account `json:"account"`
	APIHost   string  `json:"api_host"`
	VerifySSL bool    `json:"verify_ssl"`
}

// ConfigDir 与 Python config_dir 一致：优先 MCLOUDMOUNT_HOME，否则 %APPDATA%\mCloudMount。
func ConfigDir() string {
	if base := os.Getenv("MCLOUDMOUNT_HOME"); base != "" {
		return base
	}
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		return filepath.Join(appdata, "mCloudMount")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "mCloudMount")
}

// LoadConfig 读取配置文件。
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Config{VerifySSL: true} // 缺省必须为 true，与 Python 版一致
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, errors.New("配置文件不是合法 JSON: " + err.Error())
	}
	return &cfg, nil
}

func (a *Account) ext(key string) string {
	if v, ok := a.ExtInfo[key].(string); ok {
		return v
	}
	return ""
}
