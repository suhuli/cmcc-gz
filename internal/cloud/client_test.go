package cloud_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"path/filepath"
	"testing"
	"time"

	"mcloudmount/internal/cloud"
	"mcloudmount/internal/cloud/cloudtest"
	"mcloudmount/internal/config"
)

func newClient(t *testing.T, srv *cloudtest.Server, loggedIn bool) (*cloud.Client, *config.Store) {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if loggedIn {
		err = store.Update(func(c *config.Config) error {
			c.Account.Phone = "13800000000"
			c.Account.Token = srv.Token
			c.Account.UserID = "u1"
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	c := cloud.New(store)
	c.SetPersonalURL(srv.PersonalURL())
	c.UserDomainURL = srv.UserURL()
	c.RetryBase = time.Millisecond
	return c, store
}

func TestListFolderPages(t *testing.T) {
	srv := cloudtest.New()
	defer srv.Close()
	srv.PageSize = 2
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		srv.AddFile("/", n+".txt", []byte(n))
	}
	srv.AddDir("/", "Docs")
	c, _ := newClient(t, srv, true)
	items, err := c.ListFolder(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 6 {
		t.Fatalf("want 6 items, got %d", len(items))
	}
	if srv.CallCount("hcy/file/list") != 3 {
		t.Errorf("want 3 pages, got %d", srv.CallCount("hcy/file/list"))
	}
	if !items[0].IsDir || items[0].Name != "Docs" || items[1].Size != 1 || items[1].Updated.IsZero() {
		t.Errorf("unexpected items: %+v", items[:2])
	}
}

func TestRetryOnServerBusy(t *testing.T) {
	srv := cloudtest.New()
	defer srv.Close()
	c, _ := newClient(t, srv, true)
	srv.FailNext["hcy/file/list"] = 2
	if _, err := c.ListFolder(context.Background(), "/"); err != nil {
		t.Fatalf("reads should be retried: %v", err)
	}
	// 写操作：限流/网关错误同样可以重试（服务端未执行）
	srv.FailNext["hcy/file/create"] = 1
	if _, err := c.CreateFolder(context.Background(), "/", "x"); err != nil {
		t.Fatalf("503 on write should be retried: %v", err)
	}
}

func TestAuthErrorsAndResolve(t *testing.T) {
	srv := cloudtest.New()
	defer srv.Close()
	c, store := newClient(t, srv, false)
	if _, err := c.ListFolder(context.Background(), "/"); !cloud.IsKind(err, cloud.KindAuth) {
		t.Fatalf("want auth error when logged out, got %v", err)
	}
	// 保存的 token 失效、refresh_token 可用：协商后应改用并写回
	_ = store.Update(func(cfg *config.Config) error {
		cfg.Account.Phone = "13800000000"
		cfg.Account.Token = "stale"
		cfg.Account.RefreshToken = srv.Token
		return nil
	})
	prof, _, err := c.ResolveConnection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if prof != "pc" || store.Account().Token != srv.Token {
		t.Fatalf("resolved token not persisted: %q %q", prof, store.Account().Token)
	}
	if _, err := c.ListFolder(context.Background(), "/"); err != nil {
		t.Fatalf("negotiated token should be used afterwards: %v", err)
	}
}

func TestExpiredToken(t *testing.T) {
	srv := cloudtest.New()
	defer srv.Close()
	c, store := newClient(t, srv, true)
	_ = store.Update(func(cfg *config.Config) error {
		cfg.Account.TokenExpireMs = time.Now().Add(-time.Minute).UnixMilli()
		return nil
	})
	if _, err := c.ListFolder(context.Background(), "/"); !cloud.IsKind(err, cloud.KindAuth) {
		t.Fatalf("expired token must be rejected locally, got %v", err)
	}
}

func TestFolderOps(t *testing.T) {
	srv := cloudtest.New()
	defer srv.Close()
	c, _ := newClient(t, srv, true)
	ctx := context.Background()
	d, err := c.CreateFolder(ctx, "/", "工作")
	if err != nil || d.ID == "" || d.Name != "工作" {
		t.Fatalf("create: %+v %v", d, err)
	}
	f := srv.AddFile("/", "a.txt", []byte("hi"))
	if err := c.Rename(ctx, f, "b.txt"); err != nil {
		t.Fatal(err)
	}
	srv.AddFile("/", "c.txt", nil)
	if err := c.Rename(ctx, f, "c.txt"); !cloud.IsKind(err, cloud.KindConflict) {
		t.Fatalf("rename onto existing should conflict, got %v", err)
	}
	if err := c.Move(ctx, []string{f}, d.ID); err != nil {
		t.Fatal(err)
	}
	if srv.Find("/工作/b.txt") == nil {
		t.Fatal("move failed")
	}
	if err := c.Trash(ctx, []string{d.ID}); err != nil {
		t.Fatal(err)
	}
	if srv.Find("/工作") != nil {
		t.Fatal("trash failed")
	}
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestUploadAndDownload(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      int
		urlsFirst bool
	}{{"empty", 0, true}, {"small", 1000, true}, {"multipart", int(cloud.PartSize(1))*2 + 123, true}, {"urls-later", 5000, false}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := cloudtest.New()
			defer srv.Close()
			srv.PartURLsInCreate = tc.urlsFirst
			c, _ := newClient(t, srv, true)
			data := bytes.Repeat([]byte("0123456789abcdef"), tc.size/16+1)[:tc.size]
			var progress int64
			res, err := c.Upload(context.Background(), cloud.UploadRequest{
				ParentID: "/", Name: "f.bin", Size: int64(len(data)), SHA256: sha(data),
				Source: bytes.NewReader(data), Progress: func(n int64) { progress = n },
			})
			if err != nil {
				t.Fatal(err)
			}
			if progress != int64(len(data)) {
				t.Errorf("progress %d != %d", progress, len(data))
			}
			n := srv.Find("/f.bin")
			if n == nil || !bytes.Equal(n.Data, data) || n.ID != res.FileID {
				t.Fatal("uploaded content mismatch")
			}
			for _, off := range []int64{0, int64(len(data) / 2)} {
				rc, err := c.OpenFile(context.Background(), res.FileID, off)
				if err != nil {
					t.Fatal(err)
				}
				got, _ := io.ReadAll(rc)
				rc.Close()
				if !bytes.Equal(got, data[off:]) {
					t.Fatalf("download from %d mismatch", off)
				}
			}
		})
	}
}

func TestUploadRetriesPartAndRapid(t *testing.T) {
	srv := cloudtest.New()
	defer srv.Close()
	c, _ := newClient(t, srv, true)
	data := []byte("hello world")
	srv.FailNext["upload"] = 2
	if _, err := c.Upload(context.Background(), cloud.UploadRequest{ParentID: "/", Name: "a", Size: int64(len(data)), SHA256: sha(data), Source: bytes.NewReader(data)}); err != nil {
		t.Fatalf("part upload should be retried: %v", err)
	}
	before := srv.CallCount("upload")
	res, err := c.Upload(context.Background(), cloud.UploadRequest{ParentID: "/", Name: "b", Size: int64(len(data)), SHA256: sha(data), Source: bytes.NewReader(data)})
	if err != nil || !res.Rapid || srv.CallCount("upload") != before {
		t.Fatalf("expected rapid upload without data transfer: %+v %v", res, err)
	}
}

func TestDownloadRangeIgnored(t *testing.T) {
	srv := cloudtest.New()
	defer srv.Close()
	srv.RangeIgnored = true
	c, _ := newClient(t, srv, true)
	id := srv.AddFile("/", "x", []byte("0123456789"))
	rc, err := c.OpenFile(context.Background(), id, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "456789" {
		t.Fatalf("got %q", got)
	}
	// 下载地址应被缓存
	_, _ = c.OpenFile(context.Background(), id, 0)
	if n := srv.CallCount("hcy/file/getDownloadUrl"); n != 1 {
		t.Errorf("download url should be cached, fetched %d times", n)
	}
}

func TestLoginFlow(t *testing.T) {
	srv := cloudtest.New()
	defer srv.Close()
	c, store := newClient(t, srv, false)
	ctx := context.Background()
	if err := c.SendSMSCode(ctx, "123"); err == nil {
		t.Fatal("invalid phone accepted")
	}
	if err := c.SendSMSCode(ctx, "13800000000"); err != nil {
		t.Fatal(err)
	}
	if err := c.Login(ctx, "13800000000", "000000"); err == nil {
		t.Fatal("wrong code accepted")
	}
	if err := c.Login(ctx, "13800000000", srv.SMSCode); err != nil {
		t.Fatal(err)
	}
	a := store.Account()
	if a.Token != srv.Token || a.UserID != "u1" || a.DeviceID == "" || a.TokenExpireMs <= time.Now().UnixMilli() || a.Ext("profile") != "pc" {
		t.Fatalf("account not saved correctly: %+v", a)
	}
}
