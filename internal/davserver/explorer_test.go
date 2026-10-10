package davserver

import (
	"bytes"
	"crypto/rand"
	"io"
	"strings"
	"testing"
	"time"
)

// TestExplorerCopyKeepsContent 模拟资源管理器复制大文件的完整请求序列。
// 回归：PROPPATCH 以 O_RDWR 打开文件，曾被当成写入，关闭时用 0 字节覆盖了刚上传的内容。
func TestExplorerCopyKeepsContent(t *testing.T) {
	e := newEnv(t)
	data := make([]byte, 40<<20+12345) // 多个分片
	rand.Read(data)
	// 资源管理器复制：PUT 0 字节 → LOCK → PUT 内容 → PROPPATCH → UNLOCK
	e.expect(e.do("PUT", "/setup.exe", []byte{}, nil), 201, 204)
	lock := `<?xml version="1.0" encoding="utf-8" ?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner><D:href>u</D:href></D:owner></D:lockinfo>`
	resp := e.do("LOCK", "/setup.exe", []byte(lock), map[string]string{"Timeout": "Second-3600", "Content-Type": "text/xml"})
	tok := resp.Header.Get("Lock-Token")
	e.expect(resp, 200, 201)
	start := time.Now()
	e.expect(e.do("PUT", "/setup.exe", data, map[string]string{"If": "(" + tok + ")", "Content-Type": "application/x-msdownload"}), 201, 204)
	t.Logf("PUT took %v", time.Since(start))
	pp := `<?xml version="1.0" encoding="utf-8" ?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:"><D:set><D:prop><Z:Win32LastModifiedTime>Wed, 10 Oct 2026 03:00:00 GMT</Z:Win32LastModifiedTime><Z:Win32FileAttributes>00000020</Z:Win32FileAttributes></D:prop></D:set></D:propertyupdate>`
	e.expect(e.do("PROPPATCH", "/setup.exe", []byte(pp), map[string]string{"If": "(" + tok + ")", "Content-Type": "text/xml"}), 207)
	e.expect(e.do("UNLOCK", "/setup.exe", nil, map[string]string{"Lock-Token": tok}), 204)
	n := e.cloud.Find("/setup.exe")
	if n == nil || !bytes.Equal(n.Data, data) {
		t.Fatalf("exe content changed after PROPPATCH")
	}
	if c := e.cloud.CallCount("hcy/file/create"); c != 1 {
		t.Fatalf("expected exactly one upload, got %d file/create calls", c)
	}
	// 再 PROPPATCH 一次（例如资源管理器修改属性），内容不应变化
	e.expect(e.do("PROPPATCH", "/setup.exe", []byte(pp), map[string]string{"Content-Type": "text/xml"}), 207)
	if n := e.cloud.Find("/setup.exe"); n == nil || !bytes.Equal(n.Data, data) {
		t.Fatal("content changed after second PROPPATCH")
	}
	// 下载 apk
	e.cloud.AddFile("/", "app.apk", data)
	e.fs.VFS().InvalidateAll()
	resp = e.do("GET", "/app.apk", nil, nil)
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(got, data) {
		t.Fatalf("apk GET status=%d err=%v len=%d", resp.StatusCode, err, len(got))
	}
	t.Logf("apk content-type=%q", resp.Header.Get("Content-Type"))
	body := e.expect(e.do("PROPFIND", "/app.apk", nil, map[string]string{"Depth": "0"}), 207)
	if !strings.Contains(body, "getcontentlength") {
		t.Fatal("no length")
	}
}
