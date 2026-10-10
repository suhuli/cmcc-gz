// probe 是 M0 的只读烟测工具：列目录、读取文件片段、协商连接。
// 不执行任何写入操作，输出中不包含令牌。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"mcloudmount/internal/cloud"
)

func main() {
	cfgPath := flag.String("config", "", "配置文件路径（默认 MCLOUDMOUNT_HOME/config.json）")
	list := flag.String("list", "", "列出目录的 fileId（根目录用 /）")
	resolve := flag.Bool("resolve", false, "协商连接并打印 profile 与地址（不输出令牌）")
	dl := flag.String("download", "", "要读取的文件 fileId")
	offset := flag.Int64("offset", 0, "读取起始偏移")
	limit := flag.Int64("limit", 1<<20, "读取字节数上限（0 表示读到结尾）")
	flag.Parse()

	if *cfgPath == "" {
		*cfgPath = filepath.Join(cloud.ConfigDir(), "config.json")
	}
	cfg, err := cloud.LoadConfig(*cfgPath)
	if err != nil {
		fail("读取配置失败: %v", err)
	}
	client := cloud.NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if *resolve {
		ep, err := client.ResolveConnection(ctx)
		if err != nil {
			fail("协商失败: %v", err)
		}
		fmt.Printf("connection ok: profile=%s base=%s\n", ep.Profile(), ep.Base())
	}
	if *list != "" {
		start := time.Now()
		items, err := client.ListFolder(ctx, *list)
		if err != nil {
			fail("列目录失败: %v", err)
		}
		fmt.Printf("%d items in %s\n", len(items), time.Since(start).Round(time.Millisecond))
		for _, it := range items {
			kind := "file"
			if it.IsDir {
				kind = "dir "
			}
			fmt.Printf("%s %12d  %-28s %s  %s\n", kind, it.Size, it.ID, it.Updated, it.Name)
		}
	}
	if *dl != "" {
		url, err := client.GetDownloadURL(ctx, *dl)
		if err != nil {
			fail("获取下载地址失败: %v", err)
		}
		body, err := client.OpenRange(ctx, url, *offset)
		if err != nil {
			fail("打开下载流失败: %v", err)
		}
		defer body.Close()
		var r io.Reader = body
		if *limit > 0 {
			r = io.LimitReader(body, *limit)
		}
		h := sha256.New()
		start := time.Now()
		n, err := io.Copy(h, r)
		if err != nil {
			fail("读取失败: %v", err)
		}
		el := time.Since(start)
		fmt.Printf("read %d bytes from offset %d in %s (%.1f MB/s) sha256=%s\n",
			n, *offset, el.Round(time.Millisecond), float64(n)/1e6/el.Seconds(), hex.EncodeToString(h.Sum(nil)))
	}
	if !*resolve && *list == "" && *dl == "" {
		fmt.Fprintln(os.Stderr, "没有指定操作：使用 -resolve、-list 或 -download")
		os.Exit(2)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
