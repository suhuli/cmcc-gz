// Package platform 封装与操作系统相关的能力：映射盘符、WebClient 服务、打开文件夹/网址、开机自启。
//
// Windows 以外的系统只提供 WebDAV 服务（可用系统自带的 WebDAV 客户端连接），挂载相关函数返回 ErrUnsupported。
package platform

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupported 表示当前系统不支持该操作。
var ErrUnsupported = errors.New("当前系统不支持此操作")

// WebClientInfo 描述 Windows WebClient 服务状态。
type WebClientInfo struct {
	Supported     bool   `json:"supported"`
	Running       bool   `json:"running"`
	AutoStart     bool   `json:"auto_start"`
	FileSizeLimit uint32 `json:"file_size_limit"` // 字节；0 表示未知
	NeedsFix      bool   `json:"needs_fix"`
}

// RecommendedFileSizeLimit 是建议的 WebClient 单文件上限（约 4 GB，即最大值）。
const RecommendedFileSizeLimit = 0xFFFFFFFF

// DefaultFileSizeLimit 是 Windows 默认的单文件上限（50 MB）。
const DefaultFileSizeLimit = 50000000

// UNC 返回 WebDAV 根目录对应的 UNC 路径。
func UNC(host string, port int) string {
	return fmt.Sprintf(`\\%s@%d\DavWWWRoot`, host, port)
}

// IsOurMapping 判断某个盘符映射是否指向本程序的 WebDAV 服务。
func IsOurMapping(remote, host string, port int) bool {
	r := strings.ToLower(strings.TrimRight(remote, `\`))
	prefix := strings.ToLower(fmt.Sprintf(`\\%s@%d`, host, port))
	return r == prefix || strings.HasPrefix(r, prefix+`\`)
}
