package davserver

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

const lockBody = `<?xml version="1.0" encoding="utf-8" ?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner><D:href>u</D:href></D:owner></D:lockinfo>`

func (e *env) lock(p string) string {
	e.t.Helper()
	resp := e.do("LOCK", p, []byte(lockBody), map[string]string{"Timeout": "Second-3600", "Content-Type": "text/xml"})
	tok := resp.Header.Get("Lock-Token")
	e.expect(resp, 200, 201)
	if !strings.HasPrefix(tok, "<opaquelocktoken:") {
		e.t.Fatalf("lock token %q is not an opaquelocktoken URI", tok)
	}
	return tok
}

// 回归：Windows WebClient 加锁后上传时没有带回可识别的令牌，曾返回 423 Locked。
func TestLockedPutWithoutIfHeader(t *testing.T) {
	e := newEnv(t)
	e.expect(e.do("PUT", "/a.exe", []byte{}, nil), 201, 204)
	e.lock("/a.exe")
	data := bytes.Repeat([]byte("MZ"), 3<<20) // 6MB
	e.expect(e.do("PUT", "/a.exe", data, nil), 201, 204)
	if n := e.cloud.Find("/a.exe"); n == nil || !bytes.Equal(n.Data, data) {
		t.Fatal("content mismatch")
	}
}

func TestLockedPutWithForeignIfHeaders(t *testing.T) {
	e := newEnv(t)
	tok := e.lock("/b.apk")
	for i, ifh := range []string{
		"(" + tok + ")",
		"<http://127.0.0.1@8380/b.apk> (" + tok + ")",
		"<http://localhost:8380/b.apk> (" + tok + ")",
		"(<opaquelocktoken:unknown>)",
		"(<1>)",
	} {
		data := bytes.Repeat([]byte{byte('a' + i)}, 1000+i)
		e.expect(e.do("PUT", "/b.apk", data, map[string]string{"If": ifh}), 201, 204)
		if n := e.cloud.Find("/b.apk"); n == nil || !bytes.Equal(n.Data, data) {
			t.Fatalf("If %q: content mismatch", ifh)
		}
	}
}

func TestConcurrentRequestsSameLock(t *testing.T) {
	e := newEnv(t)
	e.expect(e.do("PUT", "/c.bin", []byte("x"), nil), 201, 204)
	tok := e.lock("/c.bin")
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp := e.do("PROPPATCH", "/c.bin", []byte(`<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:"><D:set><D:prop><Z:Win32FileAttributes>00000020</Z:Win32FileAttributes></D:prop></D:set></D:propertyupdate>`), map[string]string{"If": "(" + tok + ")"})
			resp.Body.Close()
			if resp.StatusCode != 207 {
				errs <- "PROPPATCH " + resp.Status
			}
		}()
		go func() {
			defer wg.Done()
			resp := e.do("PUT", "/c.bin", bytes.Repeat([]byte("y"), 200000), map[string]string{"If": "(" + tok + ")"})
			resp.Body.Close()
			if resp.StatusCode != 201 && resp.StatusCode != 204 {
				errs <- "PUT " + resp.Status
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestLockRefreshAndUnlock(t *testing.T) {
	e := newEnv(t)
	tok := e.lock("/d.txt")
	// 刷新：带 If 头、无请求体
	e.expect(e.do("LOCK", "/d.txt", nil, map[string]string{"If": "(" + tok + ")", "Timeout": "Second-600"}), 200)
	e.expect(e.do("UNLOCK", "/d.txt", nil, map[string]string{"Lock-Token": tok}), 204)
	// 未知令牌解锁也成功
	e.expect(e.do("UNLOCK", "/d.txt", nil, map[string]string{"Lock-Token": "<opaquelocktoken:gone>"}), 204)
	// 未知令牌刷新：412，客户端会重新加锁
	e.expect(e.do("LOCK", "/d.txt", nil, map[string]string{"If": "(<opaquelocktoken:gone>)"}), 412)
}

func TestMalformedIfHeaderIgnored(t *testing.T) {
	e := newEnv(t)
	e.expect(e.do("PUT", "/e.txt", []byte("e"), map[string]string{"If": "garbage <<"}), 201, 204)
	e.expect(e.do("DELETE", "/e.txt", nil, map[string]string{"If": "(<opaquelocktoken:x>)"}), 204)
}
