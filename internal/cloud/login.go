package cloud

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"

	"mcloudmount/internal/config"
)

var phonePattern = regexp.MustCompile(`^1\d{10}$`)

// ValidPhone 校验中国大陆手机号（11 位，1 开头）。
func ValidPhone(phone string) bool { return phonePattern.MatchString(phone) }

// ValidSMSCode 校验短信验证码（4-8 位数字）。
func ValidSMSCode(code string) bool {
	return len(code) >= 4 && len(code) <= 8 && strings.Trim(code, "0123456789") == ""
}

// SendSMSCode 请求短信验证码。
func (c *Client) SendSMSCode(ctx context.Context, phone string) error {
	if !ValidPhone(phone) {
		return &APIError{Kind: KindAuth, Message: "请输入 11 位中国大陆手机号"}
	}
	clear, err := compactJSON(map[string]any{
		"phoneNumber": phone,
		"reqType":     "3",
		"random":      strconv.FormatInt(c.Now().UnixMilli(), 10),
		"nationCode":  "+86",
		"clientType":  "414",
		"mode":        "0",
	})
	if err != nil {
		return err
	}
	_, err = c.postUserDomainClear(ctx, "sms/getSmsCode", clear, phone)
	return err
}

// Login 用短信验证码登录，成功后保存登录态并协商文件协议。
func (c *Client) Login(ctx context.Context, phone, code string) error {
	res, err := c.LoginWithSMS(ctx, phone, code)
	if err != nil {
		return err
	}
	if !LoginSucceeded(res) {
		return &APIError{Kind: KindAuth, Message: "登录失败，请确认验证码是否正确（返回码 " + toText(res["return"]) + "）"}
	}
	if err := c.SaveLogin(phone, res); err != nil {
		return err
	}
	_, _, err = c.ResolveConnection(ctx)
	return err
}

// LoginWithSMS 用验证码登录，返回登录数据（含 authToken、token 等）。
func (c *Client) LoginWithSMS(ctx context.Context, phone, code string) (map[string]any, error) {
	if !ValidPhone(phone) {
		return nil, &APIError{Kind: KindAuth, Message: "请输入 11 位中国大陆手机号"}
	}
	if !ValidSMSCode(code) {
		return nil, &APIError{Kind: KindAuth, Message: "请输入正确的短信验证码"}
	}
	clear, err := compactJSON(map[string]any{
		"msisdn":     phone,
		"clienttype": "414",
		"dycpwd":     code,
		"pintype":    "8",
		"version":    "30131116",
		"cpid":       "58",
		"loginMode":  "0",
		"extInfo":    map[string]any{},
	})
	if err != nil {
		return nil, err
	}
	res, err := c.postUserDomainClear(ctx, "thirdlogin", clear, phone)
	if err != nil {
		return nil, err
	}
	if _, ok := res["account"]; !ok {
		res["account"] = phone
	}
	return res, nil
}

// LoginSucceeded 对应旧版 login_result_success 的成功码集合。
func LoginSucceeded(res map[string]any) bool {
	code := ""
	for _, k := range []string{"return", "returnResult", "code"} {
		if v, ok := res[k]; ok {
			code = toText(v)
			break
		}
	}
	switch code {
	case "0", "0000", "200059525", "200059526", "200059537", "200059538", "200059540":
		return true
	}
	return false
}

func toText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	}
	return 0, false
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SaveLogin 对应旧版 save_account：把登录结果写入配置。
func (c *Client) SaveLogin(phone string, res map[string]any) error {
	nowMs := c.Now().UnixMilli()
	var serverinfo map[string]any
	err := c.store.Update(func(cfg *config.Config) error {
		a := &cfg.Account
		a.Phone = phone
		a.Account = firstText(res, "account")
		if a.Account == "" {
			a.Account = phone
		}
		a.Token = firstText(res, "authToken", "token")
		a.RefreshToken = firstText(res, "token")
		a.UserID = firstText(res, "userid", "userDomainId")
		a.DeviceID = firstText(res, "deviceId")
		if a.DeviceID == "" {
			a.DeviceID = randomHex(16)
		}
		a.LoginID = firstText(res, "loginid")
		a.TokenExpireMs = 0
		if exp, ok := toFloat(res["atExpiretime"]); ok && exp > 0 {
			switch {
			case exp > 1e12:
				a.TokenExpireMs = int64(exp)
			case exp >= 1e9:
				a.TokenExpireMs = int64(exp * 1000)
			default:
				a.TokenExpireMs = nowMs + int64(math.Round(exp*1000))
			}
		}
		if a.ServerInfo == nil {
			a.ServerInfo = map[string]any{}
		}
		if router, ok := res["routerInfo"].([]any); ok {
			a.ServerInfo["routerInfo"] = router
		}
		if si, ok := res["serverinfo"].(map[string]any); ok {
			for k, v := range si {
				a.ServerInfo[k] = v
			}
		}
		if a.ExtInfo == nil {
			a.ExtInfo = map[string]any{}
		}
		if a.Token == "" {
			return &APIError{Kind: KindAuth, Message: "登录响应中没有令牌"}
		}
		serverinfo = a.ServerInfo
		return nil
	})
	if err != nil {
		return err
	}
	c.applyRouterInfo(serverinfo)
	return nil
}

func firstText(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := toText(m[k]); s != "" {
			return s
		}
	}
	return ""
}

// postUserDomainClear 加密请求体，解密并校验登录服务响应。
func (c *Client) postUserDomainClear(ctx context.Context, path, clear, phone string) (map[string]any, error) {
	body, err := EncryptTransport(clear, nil)
	if err != nil {
		return nil, err
	}
	h, err := userDomainHeaders(clear, phone, c.Now())
	if err != nil {
		return nil, err
	}
	data, status, err := c.post(ctx, joinURL(c.UserDomainURL, path), h, body)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(data))
	if status >= 400 || text == "" {
		return nil, &APIError{Kind: KindAPI, Message: "登录服务请求失败", HTTPStatus: status}
	}
	plain, err := DecryptTransport(text)
	if err != nil {
		return nil, &APIError{Kind: KindAPI, Message: "登录响应解密失败", HTTPStatus: status}
	}
	env, err := decodeMap([]byte(plain))
	if err != nil {
		return nil, &APIError{Kind: KindAPI, Message: "登录响应不是 JSON", HTTPStatus: status}
	}
	code := toText(env["code"])
	success, _ := env["success"].(bool)
	if !success && code != "0" && code != "0000" {
		msg := firstText(env, "message", "desc")
		if msg == "" {
			msg = "登录服务返回失败"
		}
		e := statusError(code, msg)
		e.HTTPStatus = status
		return nil, e
	}
	raw, hasData := env["data"]
	if !hasData {
		return map[string]any{"return": code, "envelope": env}, nil
	}
	login, err := decodeLoginData(raw)
	if err != nil {
		return nil, &APIError{Kind: KindAPI, Message: "登录服务返回的授权数据无法解密", HTTPStatus: status}
	}
	rc := toText(login["return"])
	if rc == "" {
		rc = toText(login["code"])
	}
	if rc != "" && rc != "0" && rc != "0000" {
		msg := firstText(login, "desc", "message")
		if msg == "" {
			msg = "登录服务返回失败"
		}
		return nil, statusError(rc, msg)
	}
	if _, ok := login["return"]; !ok {
		login["return"] = "0"
		if rc != "" {
			login["return"] = rc
		}
	}
	return login, nil
}

// decodeLoginData 解析登录数据：可能是对象、JSON 文本或 AES-ECB 加密的十六进制文本。
func decodeLoginData(data any) (map[string]any, error) {
	switch v := data.(type) {
	case map[string]any:
		return v, nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil, &APIError{Kind: KindAPI, Message: "登录响应没有授权数据"}
		}
		if !strings.HasPrefix(s, "{") {
			plain, err := DecryptLoginData(s)
			if err != nil {
				return nil, err
			}
			s = plain
		}
		return decodeMap([]byte(s))
	}
	return nil, &APIError{Kind: KindAPI, Message: "登录响应没有授权数据"}
}
