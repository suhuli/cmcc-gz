package davserver

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// authenticator 实现 HTTP Digest（RFC 2617/7616, MD5, qop=auth）与 Basic 认证。
// Windows WebClient 默认禁止在非 HTTPS 上使用 Basic，Digest 无需修改注册表即可使用。
type authenticator struct {
	user, pass, realm string
	secret            []byte
	opaque            string
	now               func() time.Time
}

const nonceLifetime = 12 * time.Hour

func newAuthenticator(user, pass, realm string) *authenticator {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	op := make([]byte, 16)
	_, _ = rand.Read(op)
	return &authenticator{user: user, pass: pass, realm: realm, secret: secret, opaque: hex.EncodeToString(op), now: time.Now}
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (a *authenticator) makeNonce() string {
	ts := strconv.FormatInt(a.now().Unix(), 10)
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(ts))
	return base64.RawURLEncoding.EncodeToString([]byte(ts + ":" + hex.EncodeToString(mac.Sum(nil)[:16])))
}

// checkNonce 返回 nonce 是否由本服务签发，以及是否已过期。
func (a *authenticator) checkNonce(nonce string) (valid, stale bool) {
	raw, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil {
		return false, false
	}
	ts, sig, ok := strings.Cut(string(raw), ":")
	if !ok {
		return false, false
	}
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(ts))
	if !hmac.Equal([]byte(sig), []byte(hex.EncodeToString(mac.Sum(nil)[:16]))) {
		return false, false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false, false
	}
	if a.now().Sub(time.Unix(sec, 0)) > nonceLifetime {
		return true, true
	}
	return true, false
}

func (a *authenticator) challenge(w http.ResponseWriter, stale bool) {
	st := ""
	if stale {
		st = ", stale=true"
	}
	w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", qop="auth", nonce="%s", opaque="%s", algorithm=MD5%s`, a.realm, a.makeNonce(), a.opaque, st))
	w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Basic realm="%s", charset="UTF-8"`, a.realm))
	http.Error(w, "需要认证", http.StatusUnauthorized)
}

func eq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// check 校验请求；失败时已写入 401 响应并返回 false。
func (a *authenticator) check(w http.ResponseWriter, r *http.Request) bool {
	h := r.Header.Get("Authorization")
	scheme, rest, _ := strings.Cut(h, " ")
	switch strings.ToLower(scheme) {
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err == nil {
			u, p, _ := strings.Cut(string(raw), ":")
			if eq(u, a.user) && eq(p, a.pass) {
				return true
			}
		}
	case "digest":
		ok, stale := a.checkDigest(r.Method, parseDigest(rest))
		if ok {
			return true
		}
		if stale {
			a.challenge(w, true)
			return false
		}
	}
	a.challenge(w, false)
	return false
}

func (a *authenticator) checkDigest(method string, p map[string]string) (ok, stale bool) {
	if !eq(p["username"], a.user) || p["realm"] != a.realm || p["uri"] == "" {
		return false, false
	}
	if alg := strings.ToUpper(p["algorithm"]); alg != "" && alg != "MD5" {
		return false, false
	}
	valid, isStale := a.checkNonce(p["nonce"])
	if !valid {
		return false, false
	}
	ha1 := md5hex(a.user + ":" + a.realm + ":" + a.pass)
	ha2 := md5hex(method + ":" + p["uri"])
	var want string
	switch p["qop"] {
	case "auth":
		want = md5hex(ha1 + ":" + p["nonce"] + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
	case "":
		want = md5hex(ha1 + ":" + p["nonce"] + ":" + ha2)
	default:
		return false, false
	}
	if !eq(strings.ToLower(p["response"]), want) {
		return false, false
	}
	if isStale {
		return false, true
	}
	return true, false
}

// parseDigest 解析 Digest 认证参数（支持带引号与不带引号的值）。
func parseDigest(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,\t")
		eqIdx := strings.IndexByte(s, '=')
		if eqIdx <= 0 {
			break
		}
		k := strings.ToLower(strings.TrimSpace(s[:eqIdx]))
		s = s[eqIdx+1:]
		var v string
		if strings.HasPrefix(s, `"`) {
			s = s[1:]
			var b strings.Builder
			i := 0
			for ; i < len(s); i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					b.WriteByte(s[i])
					continue
				}
				if s[i] == '"' {
					break
				}
				b.WriteByte(s[i])
			}
			v = b.String()
			if i < len(s) {
				s = s[i+1:]
			} else {
				s = ""
			}
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			v = strings.TrimSpace(s[:end])
			s = s[end:]
		}
		out[k] = v
	}
	return out
}
