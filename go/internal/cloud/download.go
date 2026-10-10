package cloud

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// OpenRange 从 offset 开始读取下载地址的内容。
// 服务端忽略 Range 并返回 200 时，读取并丢弃前 offset 字节，与 Python 版 _open 行为一致。
func (c *Client) OpenRange(ctx context.Context, url string, offset int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = downloadHeaders()
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &APIError{Kind: KindNetwork, Message: "下载失败: " + err.Error()}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		resp.Body.Close()
		return nil, statusError("404", "文件不存在")
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		resp.Body.Close()
		return nil, statusError(fmt.Sprint(resp.StatusCode), "下载被拒绝")
	case resp.StatusCode >= 400:
		resp.Body.Close()
		return nil, &APIError{Kind: KindAPI, Message: "下载失败", HTTPStatus: resp.StatusCode}
	}
	if offset > 0 && resp.StatusCode == http.StatusOK {
		if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
			resp.Body.Close()
			return nil, &APIError{Kind: KindNetwork, Message: "跳过已读字节失败: " + err.Error()}
		}
	} else if offset > 0 && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, &APIError{Kind: KindAPI, Message: "服务端未按 Range 返回", HTTPStatus: resp.StatusCode}
	}
	return resp.Body, nil
}
