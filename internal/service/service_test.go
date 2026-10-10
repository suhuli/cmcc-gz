package service_test

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"mcloudmount/internal/cloud"
	"mcloudmount/internal/cloud/cloudtest"
	"mcloudmount/internal/config"
	"mcloudmount/internal/service"
)

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func setup(t *testing.T, loggedIn bool) (*service.Service, *config.Store) {
	t.Setenv("MCLOUDMOUNT_HOME", t.TempDir())
	srv := cloudtest.New()
	t.Cleanup(srv.Close)
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	_ = store.Update(func(c *config.Config) error {
		if loggedIn {
			c.Account.Phone, c.Account.Token, c.Account.UserID = "13800000000", srv.Token, "u1"
		}
		c.Mount.Port = port
		return nil
	})
	cl := cloud.New(store)
	cl.SetPersonalURL(srv.PersonalURL())
	cl.UserDomainURL = srv.UserURL()
	cl.RetryBase = time.Millisecond
	return service.New(store, cl), store
}

func TestStartStop(t *testing.T) {
	svc, store := setup(t, true)
	ctx := context.Background()
	if err := svc.Start(ctx, service.Options{NoMount: true}); err != nil {
		t.Fatal(err)
	}
	st := svc.Status()
	if st.Phase != service.PhaseRunning || !st.ServerRunning || st.WebDAV == "" {
		t.Fatalf("status = %+v", st)
	}
	if store.Get().Mount.DavPassword == "" {
		t.Fatal("dav password not generated")
	}
	// 未认证访问应返回 401
	resp, err := http.Get(st.WebDAV)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", resp.StatusCode)
	}
	// 重复启动无副作用
	if err := svc.Start(ctx, service.Options{NoMount: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if st := svc.Status(); st.Phase != service.PhaseIdle || st.ServerRunning {
		t.Fatalf("after stop = %+v", st)
	}
	// 端口应已释放，可以再次启动
	if err := svc.Start(ctx, service.Options{NoMount: true}); err != nil {
		t.Fatal(err)
	}
	svc.Shutdown()
}

func TestStartRequiresLogin(t *testing.T) {
	svc, _ := setup(t, false)
	err := svc.Start(context.Background(), service.Options{NoMount: true})
	if err == nil {
		t.Fatal("expected error")
	}
	if st := svc.Status(); st.Phase != service.PhaseError || st.Error == "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestStartExpired(t *testing.T) {
	svc, store := setup(t, true)
	_ = store.Update(func(c *config.Config) error {
		c.Account.TokenExpireMs = time.Now().Add(-time.Hour).UnixMilli()
		return nil
	})
	if err := svc.Start(context.Background(), service.Options{NoMount: true}); err == nil {
		t.Fatal("expected error")
	}
}

func TestPortInUse(t *testing.T) {
	svc, store := setup(t, true)
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_ = store.Update(func(c *config.Config) error {
		c.Mount.Port = ln.Addr().(*net.TCPAddr).Port
		return nil
	})
	if err := svc.Start(context.Background(), service.Options{NoMount: true}); err == nil {
		t.Fatal("expected port error")
	}
	if svc.Status().ServerRunning {
		t.Fatal("server should not run")
	}
}
