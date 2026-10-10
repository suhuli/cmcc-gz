package cloud

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"mcloudmount/internal/config"
)

const (
	DefaultPersonalURL = "https://personal-kd-njs.yun.139.com/hcy/"
	DefaultUserDomain  = "https://user-njs.yun.139.com/user"

	apiTimeout       = 30 * time.Second
	maxResponseBytes = 16 << 20
)

// Client 是中国移动云盘个人云的协议客户端，可并发使用。
type Client struct {
	store *config.Store

	api  *http.Client // JSON 接口：每个请求单独限时
	xfer *http.Client // 上传/下载：不设整体超时，由空闲超时保护

	UserDomainURL string
	Now           func() time.Time
	// RetryBase 是重试退避的基准间隔，测试中可以调小。
	RetryBase time.Duration

	mu          sync.RWMutex
	personalURL string

	urls urlCache
}

// New 创建客户端。登录态从 store 读取，协商结果与登录结果会写回 store。
func New(store *config.Store) *Client {
	cfg := store.Get()
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	newTransport := func() *http.Transport {
		return &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: !cfg.VerifySSL}, //nolint:gosec // 仅在用户显式关闭校验时生效
		}
	}
	c := &Client{
		store:         store,
		api:           &http.Client{Transport: newTransport()},
		xfer:          &http.Client{Transport: newTransport()},
		UserDomainURL: DefaultUserDomain,
		Now:           time.Now,
		RetryBase:     400 * time.Millisecond,
		personalURL:   DefaultPersonalURL,
	}
	if cfg.APIHost != "" {
		c.UserDomainURL = cfg.APIHost
	}
	c.applyRouterInfo(cfg.Account.ServerInfo)
	return c
}

// Store 返回底层配置存储。
func (c *Client) Store() *config.Store { return c.store }

// SetPersonalURL 覆盖个人云接口地址（测试或调试用）。
func (c *Client) SetPersonalURL(u string) {
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	c.mu.Lock()
	c.personalURL = u
	c.mu.Unlock()
}

// PersonalURL 返回当前使用的个人云接口地址。
func (c *Client) PersonalURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.personalURL
}

func (c *Client) applyRouterInfo(serverinfo map[string]any) {
	router, ok := serverinfo["routerInfo"].([]any)
	if !ok {
		return
	}
	for _, item := range router {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["modName"].(string)
		if name == "" {
			name, _ = m["name"].(string)
		}
		url, _ := m["httpsUrl"].(string)
		if url == "" {
			url, _ = m["httpUrl"].(string)
		}
		if strings.TrimSpace(name) == "personal" && strings.HasPrefix(url, "https://") {
			c.SetPersonalURL(strings.TrimSpace(url))
		}
	}
}

func joinURL(base, path string) string {
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + strings.TrimLeft(path, "/")
}

// compactJSON 对应 Python compact_json：无空格、保持字段顺序、不转义 HTML 字符。
func compactJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// requireToken 检查本机登录态。
func (c *Client) requireToken() (config.Account, error) {
	a := c.store.Account()
	if !a.LoggedIn() {
		return a, &APIError{Kind: KindAuth, Message: "请先完成短信登录"}
	}
	if a.Expired(c.Now()) {
		return a, &APIError{Kind: KindAuth, Message: "登录已过期，请重新登录"}
	}
	return a, nil
}

// post 发送 POST 请求并返回原始响应体。网络错误统一包装为 KindNetwork。
func (c *Client) post(ctx context.Context, url string, h http.Header, body string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header = h
	resp, err := c.api.Do(req)
	if err != nil {
		if ctxErr := context.Cause(ctx); ctxErr != nil && errors.Is(err, context.Canceled) {
			return nil, 0, ctxErr
		}
		return nil, 0, &APIError{Kind: KindNetwork, Message: "网络请求失败: " + cleanNetErr(err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, &APIError{Kind: KindNetwork, Message: "读取响应失败: " + cleanNetErr(err), HTTPStatus: resp.StatusCode}
	}
	return data, resp.StatusCode, nil
}

func cleanNetErr(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner.Error()
		}
	}
	return err.Error()
}

// envelope 是个人云接口的通用响应外壳。
type envelope struct {
	Code    json.RawMessage `json:"code"`
	Return  json.RawMessage `json:"return"`
	Success json.RawMessage `json:"success"`
	Message string          `json:"message"`
	Desc    string          `json:"desc"`
}

// rawText 把 JSON 标量转成字符串；缺失或 null 返回 ok=false。
func rawText(r json.RawMessage) (string, bool) {
	if len(r) == 0 || string(r) == "null" {
		return "", false
	}
	if r[0] == '"' {
		var s string
		if json.Unmarshal(r, &s) != nil {
			return "", false
		}
		return s, true
	}
	return string(r), true
}

// ensureOK 校验个人云响应：只有 success=true 或状态码在白名单内才算成功；
// 缺失或为空的状态码视为错误（修复旧版把空码当成功的问题）。
func ensureOK(data []byte) (map[string]any, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, &APIError{Kind: KindAPI, Message: "响应不是合法 JSON"}
	}
	if string(env.Success) == "true" {
		return decodeMap(data)
	}
	code, ok := rawText(env.Code)
	if !ok {
		code, ok = rawText(env.Return)
	}
	if ok {
		switch code {
		case "0", "0000", "200", "SUCCESS":
			return decodeMap(data)
		}
	}
	msg := env.Message
	if msg == "" {
		msg = env.Desc
	}
	if msg == "" {
		msg = "服务端返回错误"
	}
	return nil, statusError(code, msg)
}

func decodeMap(data []byte) (map[string]any, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, &APIError{Kind: KindAPI, Message: "响应不是 JSON 对象"}
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// endpoint 是一组可用的 base/profile/token。
type endpoint struct {
	base    string
	profile string
	token   string
}

func (c *Client) currentEndpoint(a config.Account) endpoint {
	profile := a.Ext("profile")
	if profile == "" {
		profile = pcProfile
	}
	return endpoint{base: c.PersonalURL(), profile: profile, token: a.Token}
}

func deviceID(a config.Account) string {
	if a.DeviceID != "" {
		return a.DeviceID
	}
	if len(a.Phone) >= 4 {
		return "web-" + a.Phone[len(a.Phone)-4:]
	}
	return "web-0000"
}

func accountName(a config.Account) string {
	if a.Account != "" {
		return a.Account
	}
	return a.Phone
}

// callOpts 控制个人云请求的重试策略。
type callOpts struct {
	// idempotent 表示重复执行没有副作用（读取类接口），网络错误时可重试。
	idempotent bool
	ep         *endpoint
}

// personalCall 发送带签名的个人云请求，按策略重试。
func (c *Client) personalCall(ctx context.Context, path string, payload any, o callOpts) (map[string]any, error) {
	a, err := c.requireToken()
	if err != nil {
		return nil, err
	}
	ep := c.currentEndpoint(a)
	if o.ep != nil {
		ep = *o.ep
	}
	clear, err := compactJSON(payload)
	if err != nil {
		return nil, err
	}
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, c.backoff(attempt)); err != nil {
				return nil, err
			}
			slog.Debug("重试云端请求", "path", path, "attempt", attempt+1, "error", lastErr)
		}
		m, err := c.personalOnce(ctx, ep, a, path, clear)
		if err == nil {
			return m, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, err
		}
		if o.idempotent && retryable(err) || rejectedBeforeExecution(err) {
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func (c *Client) personalOnce(ctx context.Context, ep endpoint, a config.Account, path, clear string) (map[string]any, error) {
	h, err := fileHeaders(ep.profile, accountName(a), ep.token, clear, deviceID(a), "v1", c.Now())
	if err != nil {
		return nil, err
	}
	data, status, err := c.post(ctx, joinURL(ep.base, path), h, clear)
	if err != nil {
		return nil, err
	}
	m, err := ensureOK(data)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			ae.HTTPStatus = status
			if ae.Code == "" && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
				ae.Kind = KindAuth
				ae.Message = "登录已失效，请重新登录"
			}
			if ae.Code == "" && status == http.StatusTooManyRequests {
				ae.Kind = KindRateLimited
			}
		}
		return nil, err
	}
	return m, nil
}

func (c *Client) backoff(attempt int) time.Duration {
	base := c.RetryBase << (attempt - 1)
	return base + time.Duration(rand.Int64N(int64(base)/2+1))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ResolveConnection 协商可用的 base/profile/token：先试已保存的组合，成功即停止。
// 协商结果会应用到后续所有请求，并在发生变化时写回配置。
func (c *Client) ResolveConnection(ctx context.Context) (profile string, base string, err error) {
	a, err := c.requireToken()
	if err != nil {
		return "", "", err
	}
	saved := a.Ext("profile")
	if saved == "" {
		saved = pcProfile
	}
	profiles := []string{saved, pcProfile, mobileProfile}
	tokens := []string{a.Token}
	if a.RefreshToken != "" && a.RefreshToken != a.Token {
		tokens = append(tokens, a.RefreshToken)
	}
	bases := []string{c.PersonalURL()}
	if bases[0] != DefaultPersonalURL {
		bases = append(bases, DefaultPersonalURL)
	}
	seen := map[string]bool{}
	var failures []string
	for _, b := range bases {
		for _, tok := range tokens {
			for _, prof := range profiles {
				key := b + "|" + prof + "|" + tok
				if seen[key] {
					continue
				}
				seen[key] = true
				ep := endpoint{base: b, profile: prof, token: tok}
				_, err := c.listPage(ctx, ep, "/", "", 1)
				if err != nil {
					if ctx.Err() != nil {
						return "", "", ctx.Err()
					}
					failures = append(failures, fmt.Sprintf("%s@%s: %s", prof, b, shortErr(err)))
					// 网络不可用时换组合没有意义，直接返回
					if IsKind(err, KindNetwork) {
						return "", "", err
					}
					continue
				}
				c.SetPersonalURL(b)
				if tok != a.Token || prof != a.Ext("profile") {
					uerr := c.store.Update(func(cfg *config.Config) error {
						cfg.Account.Token = tok
						cfg.Account.ExtInfo["profile"] = prof
						return nil
					})
					if uerr != nil {
						slog.Warn("保存协商结果失败", "error", uerr)
					}
				}
				return prof, b, nil
			}
		}
	}
	return "", "", &APIError{Kind: KindAuth, Message: "登录状态无效或已失效，请重新登录 [" + strings.Join(tail(failures, 4), "; ") + "]"}
}

func shortErr(err error) string {
	msg := UserMessage(err)
	if r := []rune(msg); len(r) > 80 {
		return string(r[:80])
	}
	return msg
}

func tail(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
