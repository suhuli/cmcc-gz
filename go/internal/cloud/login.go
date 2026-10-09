package cloud

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var phonePattern = regexp.MustCompile(`^1\d{10}$`)

// ValidPhone 校验中国大陆手机号（11 位，1 开头）。
func ValidPhone(phone string) bool { return phonePattern.MatchString(phone) }

// SendSMSCode 请求短信验证码。
func (c *Client) SendSMSCode(ctx context.Context, phone string) (map[string]any, error) {
	if !ValidPhone(phone) {
		return nil, &APIError{Kind: KindAuth, Message: "请输入 11 位中国大陆手机号"}
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
		return nil, err
	}
	return c.postUserDomainClear(ctx, "sms/getSmsCode", clear, phone)
}

// LoginWithSMS 用验证码登录，返回登录数据（含 authToken、token 等）。
func (c *Client) LoginWithSMS(ctx context.Context, phone, code string) (map[string]any, error) {
	if !ValidPhone(phone) {
		return nil, &APIError{Kind: KindAuth, Message: "请输入 11 位中国大陆手机号"}
	}
	if !(len(code) >= 4 && len(code) <= 8 && strings.Trim(code, "0123456789") == "") {
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

// LoginSucceeded 对应 Python login_result_success 的成功码集合。
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
	}
	return ""
}

// postUserDomainClear 对应 Python _post_user_domain_clear：加密请求体，解密并校验响应。
func (c *Client) postUserDomainClear(ctx context.Context, path, clear, phone string) (map[string]any, error) {
	body, err := EncryptTransport(clear, nil)
	if err != nil {
		return nil, err
	}
	h, err := userDomainHeaders(clear, phone, time.Now())
	if err != nil {
		return nil, err
	}
	data, status, err := c.post(ctx, joinURL(c.UserDomainURL, path), h, body)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(data))
	if status >= 400 || text == "" {
		return nil, &APIError{Kind: KindAPI, Message: "登录服务 HTTP 请求失败", HTTPStatus: status}
	}
	plain, err := DecryptTransport(text)
	if err != nil {
		return nil, &APIError{Kind: KindAPI, Message: "登录响应解密失败", HTTPStatus: status}
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(plain), &env); err != nil {
		return nil, &APIError{Kind: KindAPI, Message: "登录响应不是 JSON", HTTPStatus: status}
	}
	code := toText(env["code"])
	success, _ := env["success"].(bool)
	if !success && code != "0" && code != "0000" {
		msg, _ := env["message"].(string)
		if msg == "" {
			msg, _ = env["desc"].(string)
		}
		if msg == "" {
			msg = "登录服务返回失败"
		}
		return nil, statusError(code, msg)
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
		msg, _ := login["desc"].(string)
		if msg == "" {
			msg, _ = login["message"].(string)
		}
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

// decodeLoginData 对应 Python _decode_login_data。
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
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return nil, err
		}
		return m, nil
	}
	return nil, &APIError{Kind: KindAPI, Message: "登录响应没有授权数据"}
}
