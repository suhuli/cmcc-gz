// genicon 生成 Windows 程序图标素材（winres/icon.png 等），供 go-winres 嵌入 exe。
//
//	go run ./tools/genicon
package main

import (
	"log"
	"os"
	"path/filepath"

	"mcloudmount/internal/icon"
)

func main() {
	dir := "winres"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	files := map[string][]byte{
		"icon.png":   icon.PNG(256, icon.Idle),
		"icon48.png": icon.PNG(48, icon.Idle),
		"icon32.png": icon.PNG(32, icon.Idle),
		"icon16.png": icon.PNG(16, icon.Idle),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
