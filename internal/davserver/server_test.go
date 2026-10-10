package davserver

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcloudmount/internal/cloud"
	"mcloudmount/internal/cloud/cloudtest"
	"mcloudmount/internal/config"
	"mcloudmount/internal/davfs"
	"mcloudmount/internal/vfs"
)

type env struct {
	t     *testing.T
	cloud *cloudtest.Server
	fs    *davfs.FS
	http  *httptest.Server
	user  string
	pass  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	srv := cloudtest.New()
	t.Cleanup(srv.Close)
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Update(func(c *config.Config) error {
		c.Account.Phone, c.Account.Token, c.Account.UserID = "13800000000", srv.Token, "u1"
		return nil
	})
	cl := cloud.New(store)
	cl.SetPersonalURL(srv.PersonalURL())
	cl.RetryBase = time.Millisecond
	fsys, err := davfs.New(cl, vfs.New(cl), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fsys.EmptyFileDelay = 300 * time.Millisecond
	fsys.ConsistencyWait = time.Second
	ds := New(fsys, Options{Host: "127.0.0.1", Port: 0, User: "u", Password: "secret"})
	hs := httptest.NewServer(ds.Handler())
	t.Cleanup(func() { hs.Close(); fsys.Close() })
	return &env{t: t, cloud: srv, fs: fsys, http: hs, user: "u", pass: "secret"}
}

func (e *env) do(method, p string, body []byte, hdr map[string]string) *http.Response {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.http.URL+p, r)
	req.SetBasicAuth(e.user, e.pass)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func (e *env) expect(resp *http.Response, codes ...int) string {
	e.t.Helper()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, c := range codes {
		if resp.StatusCode == c {
			return string(b)
		}
	}
	e.t.Fatalf("%s %s: status %d, want %v: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, codes, b)
	return ""
}

func (e *env) dest(p string) map[string]string {
	return map[string]string{"Destination": e.http.URL + p, "Overwrite": "T"}
}

func TestPropfindDoesNotDownload(t *testing.T) {
	e := newEnv(t)
	e.cloud.AddFile("/", "报告.docx", []byte("docx"))
	e.cloud.AddFile("/", "noext", []byte("xx"))
	e.cloud.AddDir("/", "照片")
	body := e.expect(e.do("PROPFIND", "/", nil, map[string]string{"Depth": "1"}), 207)
	for _, want := range []string{"%E6%8A%A5%E5%91%8A.docx", "noext", "%E7%85%A7%E7%89%87", "getcontentlength", "application/octet-stream"} {
		if !strings.Contains(body, want) {
			t.Errorf("PROPFIND missing %q", want)
		}
	}
	if n := e.cloud.CallCount("hcy/file/getDownloadUrl") + e.cloud.CallCount("download"); n != 0 {
		t.Fatalf("PROPFIND must not download content, got %d calls", n)
	}
	// 再次 PROPFIND 命中缓存
	e.expect(e.do("PROPFIND", "/", nil, map[string]string{"Depth": "1"}), 207)
	if n := e.cloud.CallCount("hcy/file/list"); n != 1 {
		t.Errorf("listing should be cached, got %d list calls", n)
	}
}

func TestPutGetRange(t *testing.T) {
	e := newEnv(t)
	data := bytes.Repeat([]byte("abcdefghij"), 1000)
	e.expect(e.do("PUT", "/a.bin", data, nil), 201)
	if n := e.cloud.Find("/a.bin"); n == nil || !bytes.Equal(n.Data, data) {
		t.Fatal("cloud content mismatch")
	}
	got := e.expect(e.do("GET", "/a.bin", nil, nil), 200)
	if got != string(data) {
		t.Fatal("GET mismatch")
	}
	part := e.expect(e.do("GET", "/a.bin", nil, map[string]string{"Range": "bytes=5000-5009"}), 206)
	if part != string(data[5000:5010]) {
		t.Fatalf("range mismatch: %q", part)
	}
}

func TestWindowsEmptyThenContent(t *testing.T) {
	e := newEnv(t)
	e.expect(e.do("PUT", "/new.txt", []byte{}, nil), 201, 204)
	// 空文件立即可见
	e.expect(e.do("PROPFIND", "/new.txt", nil, map[string]string{"Depth": "0"}), 207)
	e.expect(e.do("PUT", "/new.txt", []byte("hello"), nil), 201, 204)
	if n := e.cloud.Find("/new.txt"); n == nil || string(n.Data) != "hello" {
		t.Fatal("content not uploaded")
	}
	if c := e.cloud.CallCount("hcy/file/create"); c != 1 {
		t.Fatalf("expected a single create (no empty placeholder + overwrite), got %d", c)
	}
	if kids := e.cloud.Children("/"); len(kids) != 1 {
		t.Fatalf("leftover files: %v", kids)
	}
}

func TestEmptyFileCreatedLater(t *testing.T) {
	e := newEnv(t)
	e.expect(e.do("PUT", "/empty.txt", []byte{}, nil), 201, 204)
	// 新建文件后立即改名（资源管理器“新建文本文档”）
	e.expect(e.do("MOVE", "/empty.txt", nil, e.dest("/笔记.txt")), 201, 204)
	if e.cloud.Find("/empty.txt") != nil {
		t.Fatal("placeholder should not be uploaded under the old name")
	}
	time.Sleep(700 * time.Millisecond)
	n := e.cloud.Find("/笔记.txt")
	if n == nil || len(n.Data) != 0 {
		t.Fatal("empty file should be created after the delay")
	}
}

func TestOverwriteIsSafe(t *testing.T) {
	e := newEnv(t)
	id := e.cloud.AddFile("/", "doc.txt", []byte("old"))
	e.expect(e.do("PUT", "/doc.txt", []byte("new content"), nil), 201, 204)
	n := e.cloud.Find("/doc.txt")
	if n == nil || string(n.Data) != "new content" || n.ID == id {
		t.Fatalf("overwrite failed: %+v", n)
	}
	if kids := e.cloud.Children("/"); len(kids) != 1 {
		t.Fatalf("temporary/backup files left behind: %v", kids)
	}
	// 列表中立即是新内容
	body := e.expect(e.do("PROPFIND", "/doc.txt", nil, map[string]string{"Depth": "0"}), 207)
	if !strings.Contains(body, "<D:getcontentlength>11</D:getcontentlength>") {
		t.Fatalf("size not updated: %s", body)
	}
}

func TestAbortedPutKeepsOriginal(t *testing.T) {
	e := newEnv(t)
	e.cloud.AddFile("/", "keep.txt", []byte("precious"))
	u, _ := url.Parse(e.http.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "PUT /keep.txt HTTP/1.1\r\nHost: %s\r\nAuthorization: Basic dTpzZWNyZXQ=\r\nContent-Length: 1000\r\n\r\npartial", u.Host)
	time.Sleep(100 * time.Millisecond)
	conn.Close()
	time.Sleep(300 * time.Millisecond)
	n := e.cloud.Find("/keep.txt")
	if n == nil || string(n.Data) != "precious" {
		t.Fatalf("aborted upload must not replace the original: %+v", n)
	}
	if c := e.cloud.CallCount("hcy/file/create"); c != 0 {
		t.Fatalf("no upload should start, got %d creates", c)
	}
}

func TestDirectoryOps(t *testing.T) {
	e := newEnv(t)
	e.expect(e.do("MKCOL", "/A", nil, nil), 201)
	e.expect(e.do("MKCOL", "/A", nil, nil), 405)
	e.expect(e.do("MKCOL", "/B", nil, nil), 201)
	e.expect(e.do("PUT", "/A/x.txt", []byte("x"), nil), 201)
	// 移动并改名
	e.expect(e.do("MOVE", "/A/x.txt", nil, e.dest("/B/y.txt")), 201)
	if e.cloud.Find("/B/y.txt") == nil || e.cloud.Find("/A/x.txt") != nil {
		t.Fatal("move+rename failed")
	}
	// 目标目录已有同名文件时移动并改名
	e.expect(e.do("PUT", "/A/y.txt", []byte("other"), nil), 201)
	e.expect(e.do("MOVE", "/A/y.txt", nil, e.dest("/B/z.txt")), 201)
	if n := e.cloud.Find("/B/z.txt"); n == nil || string(n.Data) != "other" {
		t.Fatal("move into dir with conflicting name failed")
	}
	if n := e.cloud.Find("/B/y.txt"); n == nil || string(n.Data) != "x" {
		t.Fatal("existing file in destination was damaged")
	}
	// 只改大小写
	e.expect(e.do("MOVE", "/B/z.txt", nil, e.dest("/B/Z.txt")), 201, 204)
	if n := e.cloud.Find("/B/Z.txt"); n == nil || string(n.Data) != "other" {
		t.Fatal("case-only rename lost the file")
	}
	// 复制
	e.expect(e.do("COPY", "/B/Z.txt", nil, e.dest("/A/copy.txt")), 201)
	if n := e.cloud.Find("/A/copy.txt"); n == nil || string(n.Data) != "other" {
		t.Fatal("copy failed")
	}
	// 移动目录
	e.expect(e.do("MOVE", "/B", nil, e.dest("/A/B")), 201)
	if e.cloud.Find("/A/B/y.txt") == nil {
		t.Fatal("dir move failed")
	}
	// 不能移动到自身内部
	e.expect(e.do("MOVE", "/A", nil, e.dest("/A/B/A")), 403, 409, 500)
	// 删除
	e.expect(e.do("DELETE", "/A", nil, nil), 204)
	if e.cloud.Find("/A") != nil {
		t.Fatal("delete failed")
	}
	e.expect(e.do("PROPFIND", "/A", nil, map[string]string{"Depth": "0"}), 404)
}

func TestLockAndProppatch(t *testing.T) {
	e := newEnv(t)
	e.expect(e.do("PUT", "/l.txt", []byte("l"), nil), 201)
	lockBody := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner>me</D:owner></D:lockinfo>`
	resp := e.do("LOCK", "/l.txt", []byte(lockBody), map[string]string{"Timeout": "Second-60"})
	token := resp.Header.Get("Lock-Token")
	e.expect(resp, 200)
	if token == "" {
		t.Fatal("no lock token")
	}
	// 单用户本地挂载：锁仅作兼容，未带令牌的写入也必须成功（Windows WebClient 常不带令牌）
	e.expect(e.do("PUT", "/l.txt", []byte("x"), nil), 201, 204)
	e.expect(e.do("PUT", "/l.txt", []byte("locked write"), map[string]string{"If": "(" + token + ")"}), 201, 204)
	e.expect(e.do("UNLOCK", "/l.txt", nil, map[string]string{"Lock-Token": token}), 204)

	patch := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:"><D:set><D:prop><Z:Win32LastModifiedTime>Wed, 01 Oct 2026 08:00:00 GMT</Z:Win32LastModifiedTime></D:prop></D:set></D:propertyupdate>`
	body := e.expect(e.do("PROPPATCH", "/l.txt", []byte(patch), nil), 207)
	if !strings.Contains(body, "200 OK") {
		t.Fatalf("proppatch should succeed: %s", body)
	}
	body = e.expect(e.do("PROPFIND", "/l.txt", []byte(`<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`), map[string]string{"Depth": "0"}), 207)
	if !strings.Contains(body, "Win32LastModifiedTime") {
		t.Fatalf("dead property not stored: %s", body)
	}
}

func TestAuthAndHost(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest("PROPFIND", e.http.URL+"/", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 || len(resp.Header.Values("WWW-Authenticate")) != 2 {
		t.Fatalf("expected digest+basic challenge, got %d %v", resp.StatusCode, resp.Header.Values("WWW-Authenticate"))
	}
	resp.Body.Close()
	chal := parseDigest(strings.TrimPrefix(resp.Header.Values("WWW-Authenticate")[0], "Digest "))

	digest := func(pass string) int {
		ha1 := md5h(e.user + ":" + chal["realm"] + ":" + pass)
		ha2 := md5h("PROPFIND:/")
		r := md5h(ha1 + ":" + chal["nonce"] + ":00000001:abc:auth:" + ha2)
		req, _ := http.NewRequest("PROPFIND", e.http.URL+"/", nil)
		req.Header.Set("Depth", "0")
		req.Header.Set("Authorization", fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="/", qop=auth, nc=00000001, cnonce="abc", response="%s", opaque="%s"`, e.user, chal["realm"], chal["nonce"], r, chal["opaque"]))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := digest(e.pass); c != 207 {
		t.Fatalf("digest auth failed: %d", c)
	}
	if c := digest("wrong"); c != 401 {
		t.Fatalf("wrong digest accepted: %d", c)
	}
	// 非本机 Host 被拒绝（防 DNS 重绑定）
	u, _ := url.Parse(e.http.URL)
	conn, _ := net.Dial("tcp", u.Host)
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: evil.example:80\r\nAuthorization: Basic dTpzZWNyZXQ=\r\nConnection: close\r\n\r\n")
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.Contains(line, "403") {
		t.Fatalf("foreign host should be rejected: %q", line)
	}
}

func md5h(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }
