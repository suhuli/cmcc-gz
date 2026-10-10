package davserver

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"mcloudmount/internal/cloud/cloudtest"
)

// 回归：上传曾在 PUT 请求内同步进行，云端慢时 Windows WebClient 超时并报“网络错误”。
// 现在 PUT 在数据收齐后立即返回，云端上传在后台进行；期间文件可见、可读。
func TestPutReturnsBeforeCloudUpload(t *testing.T) {
	e := newEnv(t)
	gate := make(chan struct{})
	e.cloud.SetPartGate(gate)
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	data := bytes.Repeat([]byte("APK!"), 6<<20/4) // 6MB

	done := make(chan int, 1)
	go func() {
		resp := e.do("PUT", "/app.apk", data, nil)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case code := <-done:
		if code != 201 && code != 204 {
			t.Fatalf("PUT status %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PUT 在云端上传完成前没有返回")
	}

	// 上传进行中：列表显示正确大小，读取返回完整内容
	body := e.expect(e.do("PROPFIND", "/", nil, map[string]string{"Depth": "1"}), 207)
	if !strings.Contains(body, "app.apk") || !strings.Contains(body, "<D:getcontentlength>6291456</D:getcontentlength>") {
		t.Fatalf("列表中没有待上传的文件: %s", body)
	}
	if got := e.expect(e.do("GET", "/app.apk", nil, nil), 200); got != string(data) {
		t.Fatalf("读取待上传文件内容不一致: %d 字节", len(got))
	}
	waitFor(t, func() bool {
		st := e.fs.UploadStats()
		return st.Pending == 1 && st.Items[0].Uploading
	})
	if e.cloud.Server.Find("/app.apk") != nil {
		t.Fatal("云端不应在分片完成前出现文件")
	}

	close(gate)
	if n := e.cloud.Find("/app.apk"); n == nil || !bytes.Equal(n.Data, data) {
		t.Fatal("后台上传后云端内容不一致")
	}
	if got := e.expect(e.do("GET", "/app.apk", nil, nil), 200); got != string(data) {
		t.Fatal("上传完成后读取内容不一致")
	}
}

func TestBackgroundUploadRetries(t *testing.T) {
	e := newEnv(t)
	e.cloud.SetFailParts(5) // 单次 Upload 内部重试 4 次仍失败，队列层重试后成功
	data := bytes.Repeat([]byte("x"), 300000)
	e.expect(e.do("PUT", "/r.exe", data, nil), 201, 204)
	if n := e.cloud.Find("/r.exe"); n == nil || !bytes.Equal(n.Data, data) {
		t.Fatal("重试后内容不一致")
	}
	if st := e.fs.UploadStats(); len(st.Failed) != 0 {
		t.Fatalf("不应有失败记录: %+v", st.Failed)
	}
}

func TestBackgroundUploadGivesUpAndKeepsLocalCopy(t *testing.T) {
	e := newEnv(t)
	e.cloud.SetFailParts(1 << 20)
	data := []byte("precious content")
	e.expect(e.do("PUT", "/keep.txt", data, nil), 201, 204)
	if e.cloud.Find("/keep.txt") != nil {
		t.Fatal("云端不应有文件")
	}
	st := e.fs.UploadStats()
	if len(st.Failed) != 1 || st.Failed[0].Path != "/keep.txt" || st.Failed[0].Saved == "" {
		t.Fatalf("失败记录不正确: %+v", st)
	}
	if b, err := os.ReadFile(st.Failed[0].Saved); err != nil || !bytes.Equal(b, data) {
		t.Fatalf("本地副本不正确: %v", err)
	}
	e.expect(e.do("GET", "/keep.txt", nil, nil), 404)
}

func TestQueuedOverwriteRenameDelete(t *testing.T) {
	e := newEnv(t)
	e.fs.UploadDelay = 400 * time.Millisecond
	e.expect(e.do("MKCOL", "/d", nil, nil), 201)

	// 覆盖：只上传最后一个版本
	e.expect(e.do("PUT", "/o.bin", []byte("v1"), nil), 201, 204)
	e.expect(e.do("PUT", "/o.bin", []byte("version2"), nil), 201, 204)
	// 改名 + 移动：上传到新位置
	e.expect(e.do("PUT", "/a.bin", []byte("moved"), nil), 201, 204)
	e.expect(e.do("MOVE", "/a.bin", nil, map[string]string{"Destination": e.http.URL + "/d/b.bin"}), 201, 204)
	// 删除：不上传
	e.expect(e.do("PUT", "/gone.bin", []byte("gone"), nil), 201, 204)
	e.expect(e.do("DELETE", "/gone.bin", nil, nil), 204)
	// 目录改名：其下待上传文件随之移动
	e.expect(e.do("PUT", "/d/c.bin", []byte("child"), nil), 201, 204)
	e.expect(e.do("MOVE", "/d", nil, map[string]string{"Destination": e.http.URL + "/e"}), 201, 204)

	if got := e.expect(e.do("GET", "/e/c.bin", nil, nil), 200); got != "child" {
		t.Fatalf("目录改名后读取待上传文件: %q", got)
	}
	if n := e.cloud.Find("/o.bin"); n == nil || string(n.Data) != "version2" {
		t.Fatal("覆盖结果不正确")
	}
	if n := e.cloud.Find("/e/b.bin"); n == nil || string(n.Data) != "moved" {
		t.Fatal("改名结果不正确")
	}
	if n := e.cloud.Find("/e/c.bin"); n == nil || string(n.Data) != "child" {
		t.Fatal("目录改名后子文件不正确")
	}
	if e.cloud.Find("/a.bin") != nil || e.cloud.Find("/gone.bin") != nil {
		t.Fatal("不应上传已改名/删除的文件")
	}
	if c := e.cloud.CallCount("hcy/file/create"); c != 4 { // 1 次建目录 + 3 次上传
		t.Fatalf("create 调用 %d 次，期望 4 次", c)
	}
}

// 断开挂载/退出程序时未完成的上传保存在队列中，下次启动继续。
func TestUploadQueueResumesAfterRestart(t *testing.T) {
	srv := cloudtest.New()
	t.Cleanup(srv.Close)
	qdir := t.TempDir()
	gate := make(chan struct{})
	srv.SetPartGate(gate)

	e1 := newEnvWith(t, srv, qdir)
	data := bytes.Repeat([]byte("resume"), 100000)
	e1.expect(e1.do("PUT", "/sub.bin", data, nil), 201, 204)
	waitFor(t, func() bool { st := e1.fs.UploadStats(); return st.Pending == 1 && st.Items[0].Uploading })
	e1.http.Close()
	if err := e1.fs.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(qdir + "/queue.json"); err != nil || !strings.Contains(string(b), "/sub.bin") {
		t.Fatalf("队列未持久化: %v %s", err, b)
	}
	if srv.Find("/sub.bin") != nil {
		t.Fatal("中断的上传不应出现在云端")
	}

	close(gate)
	e2 := newEnvWith(t, srv, qdir)
	if got := e2.expect(e2.do("GET", "/sub.bin", nil, nil), 200); got != string(data) {
		t.Fatal("重启后读取待上传文件不一致")
	}
	if n := e2.cloud.Find("/sub.bin"); n == nil || !bytes.Equal(n.Data, data) {
		t.Fatal("重启后未继续上传")
	}
	ents, _ := os.ReadDir(qdir)
	for _, en := range ents {
		if en.Name() != "queue.json" {
			waitFor(t, func() bool { _, err := os.Stat(qdir + "/" + en.Name()); return os.IsNotExist(err) })
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatal("等待条件超时")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
