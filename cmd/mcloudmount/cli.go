package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"mcloudmount/internal/cloud"
	"mcloudmount/internal/config"
	"mcloudmount/internal/logx"
	"mcloudmount/internal/panel"
	"mcloudmount/internal/platform"
	"mcloudmount/internal/service"
	"mcloudmount/internal/vfs"
)

var attachOnce sync.Once

func attachConsole() { attachOnce.Do(func() { platform.AttachParentConsole() }) }

func printUsage() { fmt.Printf(usage, version, config.Dir()) }

func openStore() (*config.Store, bool) {
	store, err := config.Open("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取配置失败:", err)
		return nil, false
	}
	return store, true
}

// cliLogs 为命令行子命令配置日志：WARNING 以上输出到控制台，同时写日志文件。
func cliLogs(store *config.Store, level string) {
	if level == "" {
		level = "WARNING"
	}
	logx.Setup(logx.Options{Level: level, File: logFile(), Console: true})
	_ = store
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "错误:", cloud.UserMessage(err))
	return 1
}

func readLine(prompt string) string {
	fmt.Print(prompt)
	s, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(s)
}

func cmdLogin(args []string) int {
	fl := flag.NewFlagSet("login", flag.ContinueOnError)
	phone := fl.String("phone", "", "手机号")
	if fl.Parse(args) != nil {
		return 2
	}
	store, okk := openStore()
	if !okk {
		return 1
	}
	cliLogs(store, "")
	client := cloud.New(store)
	if *phone == "" {
		*phone = readLine("手机号: ")
	}
	if !cloud.ValidPhone(*phone) {
		fmt.Fprintln(os.Stderr, "请输入 11 位中国大陆手机号")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := client.SendSMSCode(ctx, *phone)
	cancel()
	if err != nil {
		return fail(err)
	}
	fmt.Println("验证码已发送，请查收短信。")
	code := readLine("验证码: ")
	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := client.Login(ctx, *phone, code); err != nil {
		return fail(err)
	}
	fmt.Println("登录成功。")
	return 0
}

func cmdLogout(args []string) int {
	store, okk := openStore()
	if !okk {
		return 1
	}
	if panel.Probe(panelPort(store)) {
		fmt.Fprintln(os.Stderr, "后台服务正在运行，请在控制面板中退出登录，或先退出程序。")
		return 1
	}
	if err := panel.Logout(store); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	fmt.Println("已退出登录。")
	return 0
}

func panelPort(store *config.Store) int {
	if p := store.Get().PanelPort; p > 0 {
		return p
	}
	return 8390
}

// instanceRequest 向正在运行的后台实例发送请求。
func instanceRequest(store *config.Store, method, path string) (map[string]any, error) {
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", panelPort(store), path), strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MCM", "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, errNoInstance
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode >= 300 {
		if msg, _ := out["error"].(string); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return out, nil
}

func cmdStatus(args []string) int {
	fl := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fl.Bool("json", false, "以 JSON 输出")
	if fl.Parse(args) != nil {
		return 2
	}
	store, okk := openStore()
	if !okk {
		return 1
	}
	cfg := store.Get()
	acct := cfg.Account
	out := map[string]any{
		"version":   version,
		"logged_in": acct.LoggedIn(),
		"phone":     acct.Phone,
		"expired":   acct.LoggedIn() && acct.Expired(time.Now()),
		"drive":     cfg.Mount.Drive,
		"port":      cfg.Mount.Port,
		"config":    store.Path(),
		"running":   false,
	}
	if acct.TokenExpireMs > 0 {
		out["token_expire"] = time.UnixMilli(acct.TokenExpireMs).Format("2006-01-02 15:04")
	}
	if st, err := instanceRequest(store, http.MethodGet, "/api/status"); err == nil {
		out["running"] = true
		out["phase"] = st["phase"]
		out["mounted"] = st["mounted"]
		out["error"] = st["error"]
	} else {
		out["mounted"] = platform.Mounted(cfg.Mount.Drive, cfg.Mount.Host, cfg.Mount.Port)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return 0
	}
	yes := func(b any) string {
		if b == true {
			return "是"
		}
		return "否"
	}
	fmt.Println("版本:      ", version)
	if acct.LoggedIn() {
		state := "有效"
		if out["expired"] == true {
			state = "已过期，请重新登录"
		}
		fmt.Printf("账号:       %s（%s", acct.Phone, state)
		if t, okk := out["token_expire"]; okk {
			fmt.Printf("，到期 %s", t)
		}
		fmt.Println("）")
	} else {
		fmt.Println("账号:       未登录（运行 mcloudmount login）")
	}
	fmt.Println("后台服务:  ", yes(out["running"]))
	fmt.Println("盘符:      ", cfg.Mount.Drive, "已挂载:", yes(out["mounted"]))
	if e, _ := out["error"].(string); e != "" {
		fmt.Println("错误:      ", e)
	}
	fmt.Println("配置文件:  ", store.Path())
	return 0
}

func cmdMount(args []string) int {
	fl := flag.NewFlagSet("mount", flag.ContinueOnError)
	drive := fl.String("drive", "", "盘符，例如 Z:")
	webdavOnly := fl.Bool("webdav-only", false, "只启动 WebDAV 服务，不映射盘符")
	level := fl.String("log-level", "INFO", "日志级别 DEBUG/INFO/WARNING/ERROR")
	if fl.Parse(args) != nil {
		return 2
	}
	store, okk := openStore()
	if !okk {
		return 1
	}
	cliLogs(store, *level)
	if panel.Probe(panelPort(store)) {
		fmt.Fprintln(os.Stderr, "后台服务正在运行，请在托盘或控制面板中挂载，或先退出程序。")
		return 1
	}
	if *drive != "" {
		d := strings.ToUpper(*drive)
		if len(d) == 1 {
			d += ":"
		}
		if !config.ValidDrive(d) {
			fmt.Fprintln(os.Stderr, "盘符无效:", *drive)
			return 2
		}
		_ = store.Update(func(c *config.Config) error { c.Mount.Drive = d; return nil })
	}
	svc := service.New(store, cloud.New(store))
	if err := svc.Start(context.Background(), service.Options{NoMount: *webdavOnly}); err != nil {
		fmt.Fprintln(os.Stderr, "挂载失败:", err)
		return 1
	}
	st := svc.Status()
	if st.Mounted {
		fmt.Printf("已挂载到 %s，按 Ctrl+C 卸载并退出。\n", st.Drive)
	} else {
		cfg := store.Get().Mount
		fmt.Printf("WebDAV 服务: %s  用户名: %s  密码: %s\n按 Ctrl+C 停止。\n", st.WebDAV, cfg.DavUser, cfg.DavPassword)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-sig:
			fmt.Println("正在卸载…")
			svc.Shutdown()
			fmt.Println("已退出。")
			return 0
		case <-t.C:
			if s := svc.Status(); s.Phase == service.PhaseError {
				fmt.Fprintln(os.Stderr, "服务出错:", s.Error)
				svc.Shutdown()
				return 1
			}
		}
	}
}

func cmdUmount(args []string) int {
	store, okk := openStore()
	if !okk {
		return 1
	}
	if _, err := instanceRequest(store, http.MethodPost, "/api/unmount"); err == nil {
		for i := 0; i < 60; i++ {
			time.Sleep(time.Second)
			st, err := instanceRequest(store, http.MethodGet, "/api/status")
			if err != nil || (st["phase"] != "stopping" && st["mounted"] != true) {
				fmt.Println("已卸载。")
				return 0
			}
		}
		fmt.Fprintln(os.Stderr, "卸载超时，请查看控制面板。")
		return 1
	}
	cfg := store.Get().Mount
	if !platform.DriveInUse(cfg.Drive) {
		fmt.Println("盘符未挂载。")
		return 0
	}
	if !platform.IsOurMapping(platform.RemoteName(cfg.Drive), cfg.Host, cfg.Port) {
		fmt.Fprintf(os.Stderr, "盘符 %s 不是由本程序挂载的，未做修改。\n", cfg.Drive)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := platform.Unmount(ctx, cfg.Drive); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	fmt.Println("已卸载。")
	return 0
}

func cmdLs(args []string) int {
	p := "/"
	if len(args) > 0 {
		p = strings.ReplaceAll(args[0], `\`, "/")
	}
	store, okk := openStore()
	if !okk {
		return 1
	}
	cliLogs(store, "")
	if !store.Account().LoggedIn() {
		fmt.Fprintln(os.Stderr, "尚未登录，请先运行 mcloudmount login")
		return 1
	}
	client := cloud.New(store)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, _, err := client.ResolveConnection(ctx); err != nil {
		return fail(err)
	}
	v := vfs.New(client)
	dir, err := v.Resolve(ctx, p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "找不到路径:", p)
		return 1
	}
	if !dir.IsDir {
		fmt.Printf("%s  %d 字节  %s\n", dir.Name, dir.Size, dir.Mod.Local().Format("2006-01-02 15:04"))
		return 0
	}
	entries, err := v.List(ctx, dir.ID)
	if err != nil {
		return fail(err)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, e := range entries {
		size := humanSize(e.Size)
		name := e.Name
		if e.IsDir {
			size, name = "<目录>", name+"/"
		}
		mod := ""
		if !e.Mod.IsZero() {
			mod = e.Mod.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", mod, size, name)
	}
	_ = w.Flush()
	fmt.Printf("共 %d 项\n", len(entries))
	return 0
}

func humanSize(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func cmdSetupWebClient(args []string) int {
	store, okk := openStore()
	if !okk {
		return 1
	}
	cliLogs(store, "INFO")
	slog.Info("正在优化 WebClient 设置")
	if err := platform.SetupWebClient(); err != nil {
		// 以管理员身份运行时窗口是隐藏的，错误同时写入日志文件供控制面板排查
		slog.Error("setup-webclient 失败", "error", err)
		return 1
	}
	info := platform.GetWebClientInfo()
	fmt.Printf("WebClient 已优化：单文件上限 %d 字节，自动启动 %v，运行中 %v\n", info.FileSizeLimit, info.AutoStart, info.Running)
	return 0
}
