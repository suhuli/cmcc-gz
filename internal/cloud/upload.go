package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	minPartSize  = 16 << 20 // 16 MiB
	maxPartCount = 256      // 分片数上限：超过时增大分片
	urlBatchSize = 100      // 每次申请上传地址的最大分片数
)

// PartSize 根据文件大小选择分片大小（按 MiB 对齐）。
func PartSize(size int64) int64 {
	ps := int64(minPartSize)
	if size > ps*maxPartCount {
		ps = (size + maxPartCount - 1) / maxPartCount
		const mib = 1 << 20
		ps = (ps + mib - 1) / mib * mib
	}
	return ps
}

// UploadRequest 描述一次上传。
type UploadRequest struct {
	ParentID string
	Name     string
	Size     int64
	SHA256   string      // 小写十六进制；用于秒传与完整性校验
	Source   io.ReaderAt // 文件内容
	// RenameMode 同名处理：auto_rename（默认）、refuse 等。
	RenameMode string
	// Progress 可选：每上传完一个分片回调一次（累计字节数）。
	Progress func(done int64)
}

// UploadResult 是上传结果。
type UploadResult struct {
	FileID string
	Name   string
	Rapid  bool // 秒传命中，没有实际传输数据
}

type partInfo struct {
	PartNumber      int             `json:"partNumber"`
	PartSize        int64           `json:"partSize"`
	ParallelHashCtx parallelHashCtx `json:"parallelHashCtx"`
}

type parallelHashCtx struct {
	PartOffset int64 `json:"partOffset"`
}

// createFilePayload 字段顺序与旧版 upload_create 一致。
type createFilePayload struct {
	ParentFileID         string     `json:"parentFileId"`
	Name                 string     `json:"name"`
	Size                 int64      `json:"size"`
	Type                 string     `json:"type"`
	ContentType          string     `json:"contentType"`
	ContentHash          string     `json:"contentHash"`
	ContentHashAlgorithm string     `json:"contentHashAlgorithm"`
	FileRenameMode       string     `json:"fileRenameMode"`
	ParallelUpload       bool       `json:"parallelUpload"`
	PartInfos            []partInfo `json:"partInfos"`
}

type getUploadURLPayload struct {
	FileID        string         `json:"fileId"`
	UploadID      string         `json:"uploadId"`
	PartInfos     []partInfo     `json:"partInfos"`
	TransferToken string         `json:"transferToken,omitempty"` // 后端拒绝空字符串，必须省略
	UserRegion    map[string]any `json:"userRegion"`
}

type completePayload struct {
	FileID               string `json:"fileId"`
	UploadID             string `json:"uploadId"`
	ContentHash          string `json:"contentHash"`
	ContentHashAlgorithm string `json:"contentHashAlgorithm"`
	TransferToken        string `json:"transferToken,omitempty"` // 后端拒绝空字符串，必须省略
}

func planParts(size int64) []partInfo {
	ps := PartSize(size)
	n := int((size + ps - 1) / ps)
	if n < 1 {
		n = 1
	}
	parts := make([]partInfo, n)
	for i := range parts {
		off := int64(i) * ps
		sz := ps
		if off+sz > size {
			sz = size - off
		}
		parts[i] = partInfo{PartNumber: i + 1, PartSize: sz, ParallelHashCtx: parallelHashCtx{PartOffset: off}}
	}
	return parts
}

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case json.Number:
		return t.String() == "1"
	}
	return false
}

// partURLs 解析响应中的 partInfos -> 分片号到上传地址。
func partURLs(node map[string]any) map[int]string {
	out := map[int]string{}
	list, _ := node["partInfos"].([]any)
	for i, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		n := i + 1
		if s := str(m, "partNumber"); s != "" {
			if v, err := strconv.Atoi(s); err == nil {
				n = v
			}
		}
		if u := str(m, "uploadUrl", "cdnUploadUrl", "url"); u != "" {
			out[n] = u
		}
	}
	return out
}

// Upload 上传文件：创建 -> 逐片上传 -> 完成。秒传命中时直接返回。
func (c *Client) Upload(ctx context.Context, r UploadRequest) (UploadResult, error) {
	if r.RenameMode == "" {
		r.RenameMode = "auto_rename"
	}
	parts := planParts(r.Size)
	first := parts
	if len(first) > urlBatchSize {
		first = first[:urlBatchSize]
	}
	payload := createFilePayload{
		ParentFileID:         r.ParentID,
		Name:                 r.Name,
		Size:                 r.Size,
		Type:                 "file",
		ContentType:          "application/octet-stream",
		ContentHash:          r.SHA256,
		ContentHashAlgorithm: "SHA256",
		FileRenameMode:       r.RenameMode,
		PartInfos:            first,
	}
	m, err := c.personalCall(ctx, "file/create", payload, callOpts{})
	if err != nil {
		return UploadResult{}, err
	}
	node := dataNode(m)
	res := UploadResult{FileID: str(node, "fileId", "id"), Name: str(node, "fileName", "name")}
	if res.Name == "" {
		res.Name = r.Name
	}
	if res.FileID == "" {
		return res, &APIError{Kind: KindAPI, Message: "创建上传任务失败：服务端没有返回 fileId"}
	}
	uploadID := str(node, "uploadId")
	transferToken := str(node, "transferToken")
	if uploadID == "" || asBool(node["rapidUpload"]) || asBool(node["exist"]) {
		res.Rapid = true
		return res, nil
	}

	urls := partURLs(node)
	var done int64
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		if urls[p.PartNumber] == "" {
			end := i + urlBatchSize
			if end > len(parts) {
				end = len(parts)
			}
			token, fresh, err := c.requestUploadURLs(ctx, res.FileID, uploadID, transferToken, parts[i:end])
			if err != nil {
				return res, err
			}
			transferToken = token
			for k, v := range fresh {
				urls[k] = v
			}
			if urls[p.PartNumber] == "" {
				return res, &APIError{Kind: KindAPI, Message: fmt.Sprintf("服务端没有返回第 %d 片的上传地址", p.PartNumber)}
			}
		}
		if err := c.uploadPartWithRetry(ctx, r.Source, p, res.FileID, uploadID, &transferToken, urls); err != nil {
			return res, err
		}
		delete(urls, p.PartNumber)
		done += p.PartSize
		if r.Progress != nil {
			r.Progress(done)
		}
	}
	cp := completePayload{
		FileID:               res.FileID,
		UploadID:             uploadID,
		ContentHash:          r.SHA256,
		ContentHashAlgorithm: "SHA256",
		TransferToken:        transferToken,
	}
	if _, err := c.personalCall(ctx, "file/complete", cp, callOpts{idempotent: true}); err != nil {
		return res, err
	}
	return res, nil
}

func (c *Client) requestUploadURLs(ctx context.Context, fileID, uploadID, token string, parts []partInfo) (string, map[int]string, error) {
	payload := getUploadURLPayload{
		FileID:        fileID,
		UploadID:      uploadID,
		PartInfos:     parts,
		TransferToken: token,
		UserRegion:    map[string]any{},
	}
	m, err := c.personalCall(ctx, "file/getUploadUrl", payload, callOpts{idempotent: true})
	if err != nil {
		return token, nil, err
	}
	node := dataNode(m)
	if t := str(node, "transferToken"); t != "" {
		token = t
	}
	urls := partURLs(node)
	// 只请求一片且服务端没带分片号时，按请求的分片号归位
	if len(parts) == 1 && len(urls) == 1 {
		for _, u := range urls {
			urls = map[int]string{parts[0].PartNumber: u}
		}
	}
	return token, urls, nil
}

func (c *Client) uploadPartWithRetry(ctx context.Context, src io.ReaderAt, p partInfo, fileID, uploadID string, token *string, urls map[int]string) error {
	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, c.backoff(attempt)); err != nil {
				return err
			}
			slog.Info("重试上传分片", "part", p.PartNumber, "attempt", attempt+1, "error", lastErr)
			// 地址可能已过期：重新申请
			if IsKind(lastErr, KindAuth) || IsKind(lastErr, KindNotFound) || attempt >= 2 {
				t, fresh, err := c.requestUploadURLs(ctx, fileID, uploadID, *token, []partInfo{p})
				if err == nil && fresh[p.PartNumber] != "" {
					*token = t
					urls[p.PartNumber] = fresh[p.PartNumber]
				}
			}
		}
		err := c.putPart(ctx, urls[p.PartNumber], io.NewSectionReader(src, p.ParallelHashCtx.PartOffset, p.PartSize), p.PartSize)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryable(err) && !IsKind(err, KindAuth) && !IsKind(err, KindNotFound) {
			return err
		}
	}
	return lastErr
}

func (c *Client) putPart(ctx context.Context, url string, body io.Reader, size int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// 上传方向的空闲保护：长时间没有任何字节被发送则取消
	pr := &progressReader{r: body}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		last, idle := int64(-1), time.Duration(0)
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				n := pr.count()
				if n == last {
					idle += 5 * time.Second
					if idle >= transferIdleTimeout {
						cancel()
						return
					}
				} else {
					last, idle = n, 0
				}
			}
		}
	}()
	defer close(stop)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, pr)
	if err != nil {
		return err
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	resp, err := c.xfer.Do(req)
	if err != nil {
		return &APIError{Kind: KindNetwork, Message: "上传分片失败: " + cleanNetErr(err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &APIError{Kind: KindAuth, Message: "上传地址被拒绝或已过期", HTTPStatus: resp.StatusCode}
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return &APIError{Kind: KindNotFound, Message: "上传地址已失效", HTTPStatus: resp.StatusCode}
	case resp.StatusCode >= 400:
		return &APIError{Kind: KindAPI, Message: "上传分片失败", HTTPStatus: resp.StatusCode}
	}
	return nil
}

type progressReader struct {
	r io.Reader
	n atomic.Int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n.Add(int64(n))
	return n, err
}

func (p *progressReader) count() int64 { return p.n.Load() }
