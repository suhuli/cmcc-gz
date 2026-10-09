package cloud

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultPersonalURL = "https://personal-kd-njs.yun.139.com/hcy/"
	DefaultUserDomain  = "https://user-njs.yun.139.com/user"
	defaultTimeout     = 30 * time.Second
	maxResponseBytes   = 16 << 20
)

// Client 是个人云文件域的协议客户端。M0 阶段只做读取，不写回配置。
type Client struct {
	HTTP          *http.Client
	Cfg           *Config
	UserDomainURL string
	PersonalURL   string
	Now           func() time.Time
}

// NewClient 创建客户端，并应用登录响应里下发的 personal 地址。
func NewClient(cfg *Config) *Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: !cfg.VerifySSL}, //nolint:gosec // 与 Python verify_ssl 对应
	}
	c := &Client{
		HTTP:          &http.Client{Transport: tr, Timeout: 0},
		Cfg:           cfg,
		UserDomainURL: DefaultUserDomain,
		PersonalURL:   DefaultPersonalURL,
		Now:           time.Now,
	}
	if cfg.APIHost != "" {
		c.UserDomainURL = cfg.APIHost
	}
	c.applyRouterInfo(cfg.Account.ServerInfo)
	return c
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
		url, _ := m["httpsUrl"].(string)
		if name == "personal" && strings.HasPrefix(url, "https://") {
			if !strings.HasSuffix(url, "/") {
				url += "/"
			}
			c.PersonalURL = url
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

func (c *Client) requireToken() error {
	a := c.Cfg.Account
	if a.Phone == "" || a.Token == "" {
		return &APIError{Kind: KindAuth, Message: "请先完成短信登录"}
	}
	if a.TokenExpireMs > 0 && a.TokenExpireMs <= c.Now().UnixMilli() {
		return &APIError{Kind: KindAuth, Message: "令牌已过期，请重新登录"}
	}
	return nil
}

// post 发送 POST 请求并返回原始响应体。网络错误统一包装为 KindNetwork。
func (c *Client) post(ctx context.Context, url string, h http.Header, body string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header = h
	ctx2, cancel := context.WithTimeout(req.Context(), defaultTimeout)
	defer cancel()
	req = req.WithContext(ctx2)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, &APIError{Kind: KindNetwork, Message: "网络请求失败: " + err.Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, &APIError{Kind: KindNetwork, Message: "读取响应失败: " + err.Error(), HTTPStatus: resp.StatusCode}
	}
	return data, resp.StatusCode, nil
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

// ensureOK 对应 Python _ensure_ok，并修复其空码判成功的问题：
// 只有 success=true 或状态码在白名单内才算成功；缺失状态码视为错误。
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

func statusError(code, msg string) *APIError {
	e := &APIError{Kind: KindAPI, Code: code, Message: msg}
	switch code {
	case "401", "403", "200000401", "200000413":
		e.Kind = KindAuth
	case "404", "200000404":
		e.Kind = KindNotFound
	case "409", "200000409":
		e.Kind = KindConflict
	case "429", "200000429":
		e.Kind = KindRateLimited
	}
	return e
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

// endpoint 允许覆盖 base/profile/token，用于协议协商。
type endpoint struct {
	base    string
	profile string
	token   string
}

// listPayload 字段顺序必须与 Python 版 get_disk 一致，否则签名与请求体不匹配。
type listPayload struct {
	ParentFileID            string   `json:"parentFileId"`
	ParentFilePath          bool     `json:"parentFilePath"`
	PageInfo                pageInfo `json:"pageInfo"`
	OrderBy                 string   `json:"orderBy"`
	OrderDirection          string   `json:"orderDirection"`
	WorkSpaceType           string   `json:"workSpaceType"`
	Fields                  string   `json:"fields"`
	ImageThumbnailStyleList []string `json:"imageThumbnailStyleList"`
	Type                    string   `json:"type,omitempty"`
}

type pageInfo struct {
	PageSize       int    `json:"pageSize"`
	PageCursor     string `json:"pageCursor"`
	NeedTotalCount int    `json:"needTotalCount"`
}

const listFields = "thumbnailUrls,addressDetail,mediaMetaInfo,metadataAuditInfo,userTags,contentAuditInfo,starredAt,starred,localCreatedAt,localUpdatedAt"

func (c *Client) defaultEndpoint() endpoint {
	profile := c.Cfg.Account.ext("profile")
	if profile == "" {
		profile = pcProfile
	}
	return endpoint{base: c.PersonalURL, profile: profile, token: c.Cfg.Account.Token}
}

func (c *Client) deviceID() string {
	if c.Cfg.Account.DeviceID != "" {
		return c.Cfg.Account.DeviceID
	}
	phone := c.Cfg.Account.Phone
	if len(phone) >= 4 {
		return "web-" + phone[len(phone)-4:]
	}
	return "web-0000"
}

func (c *Client) account() string {
	if c.Cfg.Account.Account != "" {
		return c.Cfg.Account.Account
	}
	return c.Cfg.Account.Phone
}

// personalPost 对应 Python _post_personal：带签名的个人云 POST 请求。
func (c *Client) personalPost(ctx context.Context, ep endpoint, path string, payload any, apiVersion string) (map[string]any, error) {
	clear, err := compactJSON(payload)
	if err != nil {
		return nil, err
	}
	h, err := fileHeaders(ep.profile, c.account(), ep.token, clear, c.deviceID(), apiVersion, c.Now())
	if err != nil {
		return nil, err
	}
	data, status, err := c.post(ctx, joinURL(ep.base, path), h, clear)
	if err != nil {
		return nil, err
	}
	m, err := ensureOK(data)
	if err != nil {
		if ae, ok := err.(*APIError); ok && ae.HTTPStatus == 0 {
			ae.HTTPStatus = status
		}
		return nil, err
	}
	return m, nil
}

// FileItem 是目录中的一项。
type FileItem struct {
	ID      string
	Name    string
	IsDir   bool
	Size    int64
	Updated string
}

type rawItem struct {
	FileID         string          `json:"fileId"`
	Name           string          `json:"name"`
	Type           string          `json:"type"`
	SystemDir      bool            `json:"systemDir"`
	Size           json.RawMessage `json:"size"`
	UpdatedAt      json.RawMessage `json:"updatedAt"`
	LocalUpdatedAt json.RawMessage `json:"localUpdatedAt"`
}

func rawInt(r json.RawMessage) int64 {
	s, ok := rawText(r)
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ListPage 是一页目录内容。
type ListPage struct {
	Items      []FileItem
	NextCursor string
}

func (c *Client) getDisk(ctx context.Context, ep endpoint, parentID, cursor string, pageSize int) (*ListPage, error) {
	if err := c.requireToken(); err != nil {
		return nil, err
	}
	payload := listPayload{
		ParentFileID:            parentID,
		ParentFilePath:          false,
		PageInfo:                pageInfo{PageSize: pageSize, PageCursor: cursor, NeedTotalCount: 0},
		OrderBy:                 "name",
		OrderDirection:          "ASC",
		WorkSpaceType:           "",
		Fields:                  listFields,
		ImageThumbnailStyleList: []string{"Small", "Large"},
	}
	m, err := c.personalPostWithEndpoint(ctx, ep, "file/list", payload)
	if err != nil {
		return nil, err
	}
	return parseListPage(m), nil
}

func (c *Client) personalPostWithEndpoint(ctx context.Context, ep endpoint, path string, payload any) (map[string]any, error) {
	return c.personalPost(ctx, ep, path, payload, "v1")
}

func parseListPage(m map[string]any) *ListPage {
	page := &ListPage{}
	node, _ := m["data"].(map[string]any)
	items, _ := node["items"].([]any)
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		var r rawItem
		if json.Unmarshal(b, &r) != nil || r.FileID == "" || r.Name == "" {
			continue
		}
		kind := strings.ToLower(r.Type)
		upd, _ := rawText(r.UpdatedAt)
		if upd == "" {
			upd, _ = rawText(r.LocalUpdatedAt)
		}
		page.Items = append(page.Items, FileItem{
			ID:      r.FileID,
			Name:    r.Name,
			IsDir:   kind == "folder" || kind == "dir" || r.SystemDir,
			Size:    rawInt(r.Size),
			Updated: upd,
		})
	}
	page.NextCursor, _ = node["nextPageCursor"].(string)
	return page
}

// ListFolder 读取目录的全部项（自动翻页）。
func (c *Client) ListFolder(ctx context.Context, parentID string) ([]FileItem, error) {
	ep := c.defaultEndpoint()
	var all []FileItem
	cursor := ""
	for {
		page, err := c.getDisk(ctx, ep, parentID, cursor, 200)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, &APIError{Kind: KindAPI, Message: "文件列表分页游标没有推进"}
		}
		cursor = page.NextCursor
	}
	return all, nil
}

// GetDownloadURL 获取文件的临时下载地址。
func (c *Client) GetDownloadURL(ctx context.Context, fileID string) (string, error) {
	if err := c.requireToken(); err != nil {
		return "", err
	}
	payload := map[string]any{"fileId": fileID, "userId": c.Cfg.Account.UserID}
	m, err := c.personalPost(ctx, c.defaultEndpoint(), "file/getDownloadUrl", payload, "v1")
	if err != nil {
		return "", err
	}
	data, _ := m["data"].(map[string]any)
	url, _ := data["url"].(string)
	if url == "" {
		return "", &APIError{Kind: KindNotFound, Message: "服务端没有返回下载地址", Code: "404"}
	}
	return url, nil
}

// ResolveConnection 协商可用的 base/profile/token。
// 与 Python 版不同：先尝试已保存的组合，成功即停止；失败才依次尝试其他组合，
// 不再每次固定尝试约 12 次。
func (c *Client) ResolveConnection(ctx context.Context) (endpoint, error) {
	if err := c.requireToken(); err != nil {
		return endpoint{}, err
	}
	a := c.Cfg.Account
	saved := a.ext("profile")
	if saved == "" {
		saved = pcProfile
	}
	profiles := []string{saved, pcProfile, mobileProfile}
	tokens := []string{a.Token}
	if a.RefreshToken != "" && a.RefreshToken != a.Token {
		tokens = append(tokens, a.RefreshToken)
	}
	bases := []string{c.PersonalURL}
	if c.PersonalURL != DefaultPersonalURL {
		bases = append(bases, DefaultPersonalURL)
	}
	seen := map[string]bool{}
	var failures []string
	for _, base := range bases {
		for _, tok := range tokens {
			for _, prof := range profiles {
				key := base + "|" + prof + "|" + tok
				if seen[key] {
					continue
				}
				seen[key] = true
				ep := endpoint{base: base, profile: prof, token: tok}
				if _, err := c.getDisk(ctx, ep, "/", "", 1); err == nil {
					return ep, nil
				} else {
					failures = append(failures, fmt.Sprintf("%s@%s: %s", prof, base, shortErr(err)))
				}
			}
		}
	}
	return endpoint{}, &APIError{Kind: KindAPI, Message: "文件协议协商失败 [" + strings.Join(tail(failures, 8), "; ") + "]"}
}

func shortErr(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		if len(ae.Message) > 120 {
			return ae.Message[:120]
		}
		return ae.Message
	}
	return err.Error()
}

func tail(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// Profile 返回协商后的身份类型（pc / mobile）。
func (e endpoint) Profile() string { return e.profile }

// Base 返回协商后的接口根地址。
func (e endpoint) Base() string { return e.base }
