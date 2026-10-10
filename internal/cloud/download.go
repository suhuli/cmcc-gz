package cloud

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// transferIdleTimeout 是传输过程中允许的最长无数据时间，超过即断开，防止资源管理器卡死。
var transferIdleTimeout = 60 * time.Second

// OpenFile 从 offset 开始读取文件内容。下载地址失效（403/404/410）时自动刷新一次。
func (c *Client) OpenFile(ctx context.Context, fileID string, offset int64) (io.ReadCloser, error) {
	for attempt := 0; attempt < 2; attempt++ {
		url, err := c.GetDownloadURL(ctx, fileID)
		if err != nil {
			return nil, err
		}
		rc, err := c.OpenURL(ctx, url, offset)
		if err == nil {
			return rc, nil
		}
		c.urls.drop(fileID)
		if attempt == 0 && (IsKind(err, KindAuth) || IsKind(err, KindNotFound)) {
			continue
		}
		if attempt == 0 && retryable(err) && ctx.Err() == nil {
			if serr := sleepCtx(ctx, c.RetryBase); serr != nil {
				return nil, serr
			}
			continue
		}
		return nil, err
	}
	return nil, &APIError{Kind: KindAPI, Message: "下载失败"}
}

// OpenURL 从 offset 开始读取下载地址的内容。
// 服务端忽略 Range 并返回 200 时，读取并丢弃前 offset 字节。
func (c *Client) OpenURL(ctx context.Context, url string, offset int64) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header = downloadHeaders()
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.xfer.Do(req)
	if err != nil {
		cancel()
		return nil, &APIError{Kind: KindNetwork, Message: "下载失败: " + cleanNetErr(err)}
	}
	fail := func(e *APIError) (io.ReadCloser, error) {
		resp.Body.Close()
		cancel()
		return nil, e
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return fail(&APIError{Kind: KindNotFound, Message: "文件不存在或下载地址已失效", HTTPStatus: resp.StatusCode})
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fail(&APIError{Kind: KindAuth, Message: "下载被拒绝", HTTPStatus: resp.StatusCode})
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		resp.Body.Close()
		cancel()
		return io.NopCloser(eofReader{}), nil
	case resp.StatusCode >= 400:
		return fail(&APIError{Kind: KindAPI, Message: "下载失败", HTTPStatus: resp.StatusCode})
	}
	body := newIdleReader(resp.Body, cancel, transferIdleTimeout)
	if offset > 0 && resp.StatusCode == http.StatusOK {
		if _, err := io.CopyN(io.Discard, body, offset); err != nil {
			body.Close()
			return nil, &APIError{Kind: KindNetwork, Message: "跳过已读字节失败: " + err.Error()}
		}
	} else if offset > 0 && resp.StatusCode != http.StatusPartialContent {
		body.Close()
		return nil, &APIError{Kind: KindAPI, Message: "服务端未按 Range 返回", HTTPStatus: resp.StatusCode}
	}
	return body, nil
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// idleReader 在连续 timeout 时间没有读到数据时取消请求。
type idleReader struct {
	rc      io.ReadCloser
	cancel  context.CancelFunc
	timer   *time.Timer
	timeout time.Duration
	once    sync.Once
}

func newIdleReader(rc io.ReadCloser, cancel context.CancelFunc, timeout time.Duration) *idleReader {
	r := &idleReader{rc: rc, cancel: cancel, timeout: timeout}
	r.timer = time.AfterFunc(timeout, cancel)
	return r
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	if err != nil && err != io.EOF {
		return n, &APIError{Kind: KindNetwork, Message: "读取数据中断: " + cleanNetErr(err)}
	}
	return n, err
}

func (r *idleReader) Close() error {
	var err error
	r.once.Do(func() {
		r.timer.Stop()
		err = r.rc.Close()
		r.cancel()
	})
	return err
}
