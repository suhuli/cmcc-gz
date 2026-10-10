package panel_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcloudmount/internal/cloud"
	"mcloudmount/internal/cloud/cloudtest"
	"mcloudmount/internal/config"
	"mcloudmount/internal/logx"
	"mcloudmount/internal/panel"
	"mcloudmount/internal/service"
)

type env struct {
	t     *testing.T
	fake  *cloudtest.Server
	store *config.Store
	h     http.Handler
	quit  chan struct{}
}

func setup(t *testing.T, loggedIn bool) *env {
	t.Setenv("MCLOUDMOUNT_HOME", t.TempDir())
	fake := cloudtest.New()
	t.Cleanup(fake.Close)
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if loggedIn {
		_ = store.Update(func(c *config.Config) error {
			c.Account.Phone, c.Account.Token, c.Account.UserID = "13800000000", fake.Token, "u1"
			return nil
		})
	}
	cl := cloud.New(store)
	cl.SetPersonalURL(fake.PersonalURL())
	cl.UserDomainURL = fake.UserURL()
	cl.RetryBase = time.Millisecond
	e := &env{t: t, fake: fake, store: store, quit: make(chan struct{}, 1)}
	p := panel.New(panel.Options{
		Store: store, Service: service.New(store, cl), Logs: logx.NewRing(50), Version: "test",
		Quit: func() { e.quit <- struct{}{} },
	})
	e.h = p.Handler()
	return e
}

func (e *env) do(method, target string, body any, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:8390"+target, rd)
	if method == http.MethodPost {
		req.Header.Set("X-MCM", "1")
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestSecurity(t *testing.T) {
	e := setup(t, true)
	req := httptest.NewRequest("GET", "http://evil.example:8390/api/status", nil)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign host = %d", w.Code)
	}
	if w, _ := e.do("POST", "/api/logout", map[string]any{}, map[string]string{"X-MCM": ""}); w.Code != http.StatusForbidden {
		t.Fatalf("missing header = %d", w.Code)
	}
	if w, _ := e.do("POST", "/api/logout", map[string]any{}, map[string]string{"Origin": "http://evil.example"}); w.Code != http.StatusForbidden {
		t.Fatalf("foreign origin = %d", w.Code)
	}
	if w, _ := e.do("GET", "/api/files/download?id=x", nil, map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site download = %d", w.Code)
	}
	w, _ = e.do("GET", "/", nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "mCloudMount") || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("index = %d", w.Code)
	}
	if e.store.Account().Token == "" {
		t.Fatal("logout should not have run")
	}
}

func TestStatusAndLogin(t *testing.T) {
	e := setup(t, false)
	_, st := e.do("GET", "/api/status", nil, nil)
	if st["logged_in"] != false || st["phase"] != "idle" {
		t.Fatalf("status = %v", st)
	}
	if w, _ := e.do("POST", "/api/mount", map[string]any{}, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("mount without login = %d", w.Code)
	}
	if w, _ := e.do("POST", "/api/login/send", map[string]any{"phone": "123"}, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad phone = %d", w.Code)
	}
	if w, b := e.do("POST", "/api/login/send", map[string]any{"phone": "13800000000"}, nil); w.Code != 200 {
		t.Fatalf("send = %d %v", w.Code, b)
	}
	if w, _ := e.do("POST", "/api/login/send", map[string]any{"phone": "13800000000"}, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("resend = %d", w.Code)
	}
	_, st = e.do("GET", "/api/status", nil, nil)
	if st["pending_phone"] != "13800000000" {
		t.Fatalf("pending = %v", st["pending_phone"])
	}
	if w, _ := e.do("POST", "/api/login/verify", map[string]any{"phone": "13800000000", "code": "000000"}, nil); w.Code == 200 {
		t.Fatal("wrong code accepted")
	}
	if w, b := e.do("POST", "/api/login/verify", map[string]any{"phone": "13800000000", "code": e.fake.SMSCode}, nil); w.Code != 200 {
		t.Fatalf("verify = %d %v", w.Code, b)
	}
	_, st = e.do("GET", "/api/status", nil, nil)
	if st["logged_in"] != true || st["account"] != "138****0000" {
		t.Fatalf("status after login = %v", st)
	}
	if w, _ := e.do("POST", "/api/logout", map[string]any{}, nil); w.Code != 200 {
		t.Fatalf("logout = %d", w.Code)
	}
	if e.store.Account().LoggedIn() {
		t.Fatal("still logged in")
	}
}

func TestFiles(t *testing.T) {
	e := setup(t, true)
	e.fake.AddFile("/", "b.txt", []byte("hello"))
	e.fake.AddDir("/", "A")
	e.fake.AddFile("/", "__mcm_tmp_x", []byte("x"))

	w, b := e.do("GET", "/api/files?folder=/", nil, nil)
	if w.Code != 200 {
		t.Fatalf("list = %d %v", w.Code, b)
	}
	items := b["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["name"] != "A" {
		t.Fatalf("items = %v", items)
	}
	fileID := items[1].(map[string]any)["id"].(string)

	if w, _ := e.do("POST", "/api/files/mkdir", map[string]any{"parent": "/", "name": "bad/name"}, nil); w.Code != 400 {
		t.Fatalf("bad name = %d", w.Code)
	}
	if w, b := e.do("POST", "/api/files/mkdir", map[string]any{"parent": "/", "name": "新建"}, nil); w.Code != 200 {
		t.Fatalf("mkdir = %d %v", w.Code, b)
	}
	if w, b := e.do("POST", "/api/files/rename", map[string]any{"id": fileID, "name": "c.txt", "parent": "/"}, nil); w.Code != 200 {
		t.Fatalf("rename = %d %v", w.Code, b)
	}
	w, _ = e.do("GET", "/api/files/download?id="+fileID+"&name=c.txt&size=5", nil, nil)
	if w.Code != 200 || w.Body.String() != "hello" || !strings.Contains(w.Header().Get("Content-Disposition"), "c.txt") {
		t.Fatalf("download = %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Disposition"))
	}
	data := bytes.Repeat([]byte("0123456789"), 1000)
	w, b = e.do("POST", "/api/files/upload?parent=/&name="+"上传.bin", data, nil)
	if w.Code != 200 {
		t.Fatalf("upload = %d %v", w.Code, b)
	}
	if got := e.fake.Find("/上传.bin"); got == nil || !bytes.Equal(got.Data, data) {
		t.Fatal("uploaded content mismatch")
	}
	if w, b := e.do("POST", "/api/files/delete", map[string]any{"ids": []string{fileID}, "parent": "/"}, nil); w.Code != 200 {
		t.Fatalf("delete = %d %v", w.Code, b)
	}
	_, b = e.do("GET", "/api/files?folder=/", nil, nil)
	for _, it := range b["items"].([]any) {
		if it.(map[string]any)["id"] == fileID {
			t.Fatal("deleted file still listed")
		}
	}
}

func TestSettingsAndQuit(t *testing.T) {
	e := setup(t, true)
	if w, b := e.do("POST", "/api/settings", map[string]any{"auto_mount": true, "drive": "y", "port": 18000}, nil); w.Code != 200 {
		t.Fatalf("settings = %d %v", w.Code, b)
	}
	cfg := e.store.Get()
	if !cfg.AutoMount || cfg.Mount.Drive != "Y:" || cfg.Mount.Port != 18000 {
		t.Fatalf("cfg = %+v", cfg.Mount)
	}
	if w, _ := e.do("POST", "/api/settings", map[string]any{"port": 80}, nil); w.Code != 400 {
		t.Fatalf("bad port = %d", w.Code)
	}
	_, b := e.do("GET", "/api/settings", nil, nil)
	if b["auto_mount"] != true || b["drive"] != "Y:" {
		t.Fatalf("get settings = %v", b)
	}
	if w, _ := e.do("GET", "/api/logs", nil, nil); w.Code != 200 {
		t.Fatal("logs")
	}
	e.do("POST", "/api/quit", map[string]any{}, nil)
	select {
	case <-e.quit:
	case <-time.After(2 * time.Second):
		t.Fatal("quit not called")
	}
}
