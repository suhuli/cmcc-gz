// Package cloud 实现中国移动云盘的协议客户端（Go 版）。
//
// 签名与加密逻辑与 Python 参考实现（src/mcloudmount/pc_login.py）逐字节一致，
// 由 testdata/vectors.json 中的跨语言向量保证。
package cloud

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

const nonceChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// pyQuote 复刻 Python 的 urllib.parse.quote(value, safe="") 再做 encode_uri_component 的替换：
// 保留字母数字和 _.-~!'()，其余字节按 UTF-8 逐个编码为大写 %XX。
func pyQuote(s string) string {
	var b strings.Builder
	const hexd = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == '-', c == '~', c == '!', c == '\'', c == '(', c == ')':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&0x0F])
		}
	}
	return b.String()
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// CreateSignature 对应 Python create_signature：按字符排序后 base64，再两次 MD5。
func CreateSignature(clearBody, timestamp, nonce string) string {
	encoded := []byte(pyQuote(clearBody))
	sort.Slice(encoded, func(i, j int) bool { return encoded[i] < encoded[j] })
	b64 := base64.StdEncoding.EncodeToString(encoded)
	a := md5Hex(b64)
	b := md5Hex(timestamp + ":" + nonce)
	return strings.ToUpper(md5Hex(a + b))
}

// RandomNonce 生成 16 位随机字母数字串。
func RandomNonce() (string, error) {
	out := make([]byte, 16)
	max := big.NewInt(int64(len(nonceChars)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = nonceChars[n.Int64()]
	}
	return string(out), nil
}

// FormatTimestamp 对应 Python format_timestamp：本地时间，格式 YYYYMMDD HHMMSS。
func FormatTimestamp(t time.Time) string {
	return t.Format("20060102 150405")
}

// SignedHeader 生成 mcloud-sign 头：timestamp,nonce,signature。
func SignedHeader(clearBody string, now time.Time) (string, error) {
	ts := FormatTimestamp(now)
	nonce, err := RandomNonce()
	if err != nil {
		return "", fmt.Errorf("生成 nonce 失败: %w", err)
	}
	return ts + "," + nonce + "," + CreateSignature(clearBody, ts, nonce), nil
}
