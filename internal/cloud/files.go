package cloud

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RootID 是个人云根目录的 fileId。
const RootID = "/"

// FileItem 是目录中的一项。
type FileItem struct {
	ID       string
	ParentID string
	Name     string
	IsDir    bool
	Size     int64
	Updated  time.Time
	Created  time.Time
	Hash     string
	// UpdatedRaw 保留服务端原始时间文本，供界面展示。
	UpdatedRaw string
}

type rawItem struct {
	FileID         string          `json:"fileId"`
	ParentFileID   string          `json:"parentFileId"`
	Name           string          `json:"name"`
	Type           string          `json:"type"`
	SystemDir      bool            `json:"systemDir"`
	Size           json.RawMessage `json:"size"`
	UpdatedAt      json.RawMessage `json:"updatedAt"`
	CreatedAt      json.RawMessage `json:"createdAt"`
	LocalUpdatedAt json.RawMessage `json:"localUpdatedAt"`
	ContentHash    string          `json:"contentHash"`
}

func rawInt(r json.RawMessage) int64 {
	s, ok := rawText(r)
	if !ok {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f)
	}
	return 0
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.000-07:00",
	"2006-01-02T15:04:05.000Z0700",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05",
	"20060102150405",
	"2006-01-02",
}

// parseTime 解析服务端时间：支持 ISO 8601 文本、秒或毫秒时间戳。
func parseTime(r json.RawMessage) (time.Time, string) {
	s, ok := rawText(r)
	if !ok || s == "" {
		return time.Time{}, ""
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && !strings.ContainsAny(s, "-:T") {
		if n > 1e12 {
			return time.UnixMilli(int64(n)), s
		}
		if n > 1e8 {
			return time.Unix(int64(n), 0), s
		}
	}
	for _, layout := range timeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, s
		}
	}
	return time.Time{}, s
}

func parseItem(it any, parentID string) (FileItem, bool) {
	b, err := json.Marshal(it)
	if err != nil {
		return FileItem{}, false
	}
	var r rawItem
	if json.Unmarshal(b, &r) != nil || r.FileID == "" || r.Name == "" {
		return FileItem{}, false
	}
	kind := strings.ToLower(r.Type)
	upd, raw := parseTime(r.UpdatedAt)
	if upd.IsZero() {
		upd, raw = parseTime(r.LocalUpdatedAt)
	}
	created, _ := parseTime(r.CreatedAt)
	if parentID == "" {
		parentID = r.ParentFileID
	}
	return FileItem{
		ID:         r.FileID,
		ParentID:   parentID,
		Name:       r.Name,
		IsDir:      kind == "folder" || kind == "dir" || r.SystemDir,
		Size:       rawInt(r.Size),
		Updated:    upd,
		Created:    created,
		Hash:       r.ContentHash,
		UpdatedRaw: raw,
	}, true
}

// listPayload 字段顺序与旧版 get_disk 一致。
type listPayload struct {
	ParentFileID            string   `json:"parentFileId"`
	ParentFilePath          bool     `json:"parentFilePath"`
	PageInfo                pageInfo `json:"pageInfo"`
	OrderBy                 string   `json:"orderBy"`
	OrderDirection          string   `json:"orderDirection"`
	WorkSpaceType           string   `json:"workSpaceType"`
	Fields                  string   `json:"fields"`
	ImageThumbnailStyleList []string `json:"imageThumbnailStyleList"`
}

type pageInfo struct {
	PageSize       int    `json:"pageSize"`
	PageCursor     string `json:"pageCursor"`
	NeedTotalCount int    `json:"needTotalCount"`
}

const listFields = "thumbnailUrls,addressDetail,mediaMetaInfo,metadataAuditInfo,userTags,contentAuditInfo,starredAt,starred,localCreatedAt,localUpdatedAt"

type listPageResult struct {
	Items      []FileItem
	NextCursor string
}

func (c *Client) listPage(ctx context.Context, ep endpoint, parentID, cursor string, pageSize int) (*listPageResult, error) {
	payload := listPayload{
		ParentFileID:            parentID,
		PageInfo:                pageInfo{PageSize: pageSize, PageCursor: cursor},
		OrderBy:                 "name",
		OrderDirection:          "ASC",
		Fields:                  listFields,
		ImageThumbnailStyleList: []string{"Small", "Large"},
	}
	m, err := c.personalCall(ctx, "file/list", payload, callOpts{idempotent: true, ep: &ep})
	if err != nil {
		return nil, err
	}
	page := &listPageResult{}
	node, _ := m["data"].(map[string]any)
	items, _ := node["items"].([]any)
	for _, it := range items {
		if fi, ok := parseItem(it, parentID); ok {
			page.Items = append(page.Items, fi)
		}
	}
	page.NextCursor, _ = node["nextPageCursor"].(string)
	return page, nil
}

// ListFolder 读取目录的全部项（自动翻页）。
func (c *Client) ListFolder(ctx context.Context, parentID string) ([]FileItem, error) {
	a, err := c.requireToken()
	if err != nil {
		return nil, err
	}
	ep := c.currentEndpoint(a)
	var all []FileItem
	cursor := ""
	for {
		page, err := c.listPage(ctx, ep, parentID, cursor, 200)
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

func dataNode(m map[string]any) map[string]any {
	node, _ := m["data"].(map[string]any)
	if node == nil {
		return map[string]any{}
	}
	return node
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case json.Number:
			return v.String()
		case bool:
			return strconv.FormatBool(v)
		}
	}
	return ""
}

// CreateFolder 在 parentID 下创建目录，返回新目录信息。
func (c *Client) CreateFolder(ctx context.Context, parentID, name string) (FileItem, error) {
	payload := struct {
		ParentFileID string `json:"parentFileId"`
		Name         string `json:"name"`
		Type         string `json:"type"`
	}{parentID, name, "folder"}
	m, err := c.personalCall(ctx, "file/create", payload, callOpts{})
	if err != nil {
		return FileItem{}, err
	}
	node := dataNode(m)
	item := FileItem{
		ID:       str(node, "fileId", "id"),
		ParentID: parentID,
		Name:     str(node, "fileName", "name"),
		IsDir:    true,
		Updated:  c.Now(),
		Created:  c.Now(),
	}
	if item.Name == "" {
		item.Name = name
	}
	return item, nil
}

// Rename 重命名文件或目录。同名冲突时服务端拒绝（refuse）。
func (c *Client) Rename(ctx context.Context, fileID, newName string) error {
	payload := struct {
		FileID         string `json:"fileId"`
		Name           string `json:"name"`
		FileRenameMode string `json:"FileRenameMode"`
	}{fileID, newName, "refuse"}
	_, err := c.personalCall(ctx, "file/update", payload, callOpts{})
	return err
}

// Move 把文件或目录移动到 toParentID。
func (c *Client) Move(ctx context.Context, fileIDs []string, toParentID string) error {
	payload := struct {
		FileIDs        []string `json:"fileIds"`
		ToParentFileID string   `json:"toParentFileId"`
	}{fileIDs, toParentID}
	_, err := c.personalCall(ctx, "file/batchMove", payload, callOpts{})
	return err
}

// Trash 把文件或目录移入回收站。
func (c *Client) Trash(ctx context.Context, fileIDs []string) error {
	payload := struct {
		FileIDs []string `json:"fileIds"`
	}{fileIDs}
	_, err := c.personalCall(ctx, "recyclebin/batchTrash", payload, callOpts{})
	for _, id := range fileIDs {
		c.urls.drop(id)
	}
	return err
}

// urlCache 缓存下载地址，避免每次打开文件都请求一次。
type urlCache struct {
	mu sync.Mutex
	m  map[string]cachedURL
}

type cachedURL struct {
	url     string
	expires time.Time
}

const downloadURLTTL = 10 * time.Minute

func (u *urlCache) get(id string, now time.Time) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	e, ok := u.m[id]
	if !ok || now.After(e.expires) {
		return "", false
	}
	return e.url, true
}

func (u *urlCache) put(id, url string, now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.m == nil {
		u.m = map[string]cachedURL{}
	}
	if len(u.m) > 4096 {
		for k, v := range u.m {
			if now.After(v.expires) {
				delete(u.m, k)
			}
		}
		if len(u.m) > 4096 {
			u.m = map[string]cachedURL{}
		}
	}
	u.m[id] = cachedURL{url: url, expires: now.Add(downloadURLTTL)}
}

func (u *urlCache) drop(id string) {
	u.mu.Lock()
	delete(u.m, id)
	u.mu.Unlock()
}

// GetDownloadURL 获取文件的临时下载地址（带缓存）。
func (c *Client) GetDownloadURL(ctx context.Context, fileID string) (string, error) {
	if u, ok := c.urls.get(fileID, c.Now()); ok {
		return u, nil
	}
	a, err := c.requireToken()
	if err != nil {
		return "", err
	}
	payload := struct {
		FileID string `json:"fileId"`
		UserID string `json:"userId"`
	}{fileID, a.UserID}
	m, err := c.personalCall(ctx, "file/getDownloadUrl", payload, callOpts{idempotent: true})
	if err != nil {
		return "", err
	}
	node := dataNode(m)
	u := str(node, "url", "cdnUrl")
	if u == "" {
		return "", &APIError{Kind: KindNotFound, Message: "服务端没有返回下载地址", Code: "404"}
	}
	c.urls.put(fileID, u, c.Now())
	return u, nil
}
