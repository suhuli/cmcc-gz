package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeCloud 模拟个人云的 file/list、file/getDownloadUrl 与下载地址。
type fakeCloud struct {
	t           *testing.T
	content     []byte
	ignoreRange bool
	listCalls   int
}

func (f *fakeCloud) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/hcy/file/list", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			ParentFileID string `json:"parentFileId"`
			PageInfo     struct {
				PageCursor string `json:"pageCursor"`
			} `json:"pageInfo"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			f.t.Errorf("list body not json: %v", err)
		}
		if !strings.Contains(r.Header.Get("mcloud-sign"), ",") {
			f.t.Errorf("missing mcloud-sign")
		}
		f.listCalls++
		w.Header().Set("Content-Type", "application/json")
		if p.PageInfo.PageCursor == "" {
			fmt.Fprint(w, `{"code":"0","data":{"items":[
				{"fileId":"d1","name":"Docs","type":"folder","size":0,"updatedAt":"2026-10-01"},
				{"fileId":"f1","name":"a.txt","type":"file","size":"12","updatedAt":1700000000},
				{"fileId":"","name":"skipped","type":"file"}
			],"nextPageCursor":"c2"}}`)
			return
		}
		fmt.Fprint(w, `{"code":"0","data":{"items":[{"fileId":"f2","name":"b.bin","type":"file","size":5}],"nextPageCursor":""}}`)
	})
	mux.HandleFunc("/hcy/file/getDownloadUrl", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":"0","data":{"url":"http://%s/dl"}}`, r.Host)
	})
	mux.HandleFunc("/dl", func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" || f.ignoreRange {
			w.Header().Set("Content-Length", fmt.Sprint(len(f.content)))
			w.Write(f.content)
			return
		}
		var start int
		fmt.Sscanf(rng, "bytes=%d-", &start)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(f.content)-1, len(f.content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(f.content[start:])
	})
	return mux
}

func newTestClient(t *testing.T, f *fakeCloud) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	cfg := &Config{VerifySSL: true, Account: Account{
		Phone: "13800000000", Account: "13800000000", Token: "tok", UserID: "u1",
		ExtInfo: map[string]any{"profile": "pc"},
	}}
	c := NewClient(cfg)
	c.PersonalURL = srv.URL + "/hcy/"
	c.Now = func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local) }
	return c
}

func TestListFolderPagesAndFilters(t *testing.T) {
	f := &fakeCloud{t: t}
	c := newTestClient(t, f)
	items, err := c.ListFolder(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if f.listCalls != 2 {
		t.Errorf("expected 2 pages, got %d", f.listCalls)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items (empty id skipped), got %d: %+v", len(items), items)
	}
	if !items[0].IsDir || items[0].Name != "Docs" {
		t.Errorf("folder not detected: %+v", items[0])
	}
	if items[1].Size != 12 || items[1].Updated != "1700000000" {
		t.Errorf("string size / numeric updatedAt not parsed: %+v", items[1])
	}
}

func TestEnsureOKRejectsEmptyCode(t *testing.T) {
	cases := map[string]bool{
		`{"code":"0","data":{}}`:        true,
		`{"success":true}`:              true,
		`{"code":"","data":{}}`:         false, // Python 版会误判为成功，这里修复
		`{"code":"401","message":"no"}`: false,
		`{"data":{}}`:                   false,
	}
	for body, wantOK := range cases {
		_, err := ensureOK([]byte(body))
		if (err == nil) != wantOK {
			t.Errorf("ensureOK(%s) err=%v, want ok=%v", body, err, wantOK)
		}
	}
	_, err := ensureOK([]byte(`{"code":"401","message":"no"}`))
	if !IsKind(err, KindAuth) {
		t.Errorf("401 should map to auth kind, got %v", err)
	}
}

func TestDownloadURLAndRange(t *testing.T) {
	content := []byte("0123456789abcdef")
	f := &fakeCloud{t: t, content: content}
	c := newTestClient(t, f)
	url, err := c.GetDownloadURL(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, 4} {
		rc, err := c.OpenRange(context.Background(), url, off)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != string(content[off:]) {
			t.Errorf("offset %d: got %q", off, got)
		}
	}
}

func TestDownloadRangeIgnoredByServer(t *testing.T) {
	content := []byte("0123456789abcdef")
	f := &fakeCloud{t: t, content: content, ignoreRange: true}
	c := newTestClient(t, f)
	url, err := c.GetDownloadURL(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.OpenRange(context.Background(), url, 6)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "6789abcdef" {
		t.Errorf("server ignored Range; expected skip, got %q", got)
	}
}

func TestRequireTokenExpired(t *testing.T) {
	f := &fakeCloud{t: t}
	c := newTestClient(t, f)
	c.Cfg.Account.TokenExpireMs = c.Now().UnixMilli() - 1
	if _, err := c.ListFolder(context.Background(), "/"); !IsKind(err, KindAuth) {
		t.Errorf("expired token should be auth error, got %v", err)
	}
}

func TestResolveConnectionStopsAtFirstSuccess(t *testing.T) {
	f := &fakeCloud{t: t}
	c := newTestClient(t, f)
	ep, err := c.ResolveConnection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ep.Profile() != "pc" {
		t.Errorf("profile = %s", ep.Profile())
	}
	if f.listCalls != 1 {
		t.Errorf("expected single probe request, got %d", f.listCalls)
	}
}

func TestLoginRejectsBadPhoneAndCode(t *testing.T) {
	c := newTestClient(t, &fakeCloud{t: t})
	if _, err := c.SendSMSCode(context.Background(), "12345"); err == nil {
		t.Error("bad phone accepted")
	}
	if _, err := c.LoginWithSMS(context.Background(), "13800000000", "12a4"); err == nil {
		t.Error("non-digit code accepted")
	}
	if ValidPhone("23800000000") || !ValidPhone("17671400063") {
		t.Error("phone validation wrong")
	}
}
