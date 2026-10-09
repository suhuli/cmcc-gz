package cloud

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"time"
)

const (
	webVersion    = "7.17.9"
	webChannel    = "10000034"
	pcProfile     = "pc"
	mobileProfile = "mobile"
)

func webDeviceInfo(deviceID string) string {
	return fmt.Sprintf("||9|%s|mobile|Android|%s||Android 14||zh-CN|||", webVersion, deviceID)
}

func webClientInfo(deviceID string) string {
	return webDeviceInfo(deviceID) + base64.StdEncoding.EncodeToString([]byte("mobile")) + "||"
}

func basicAuth(kind, account, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(kind+":"+account+":"+token))
}

// fileHeaders 对应 Python file_headers：个人云文件接口的请求头。
func fileHeaders(profile, account, token, clearBody, deviceID, apiVersion string, now time.Time) (http.Header, error) {
	sign, err := SignedHeader(clearBody, now)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	set := func(k, v string) { h.Set(k, v) }
	set("Authorization", basicAuth(profile, account, token))
	set("Accept", "application/json")
	set("Accept-Language", "zh-CN")
	set("Connection", "keep-alive")
	set("Content-Type", "application/json; charset=UTF-8")
	set("CMS-DEVICE", "default")
	set("X-Deviceinfo", webDeviceInfo(deviceID))
	set("x-yun-net-type", "wifi")
	set("x-NetType", "wifi")
	set("x-yun-client-info", webClientInfo(deviceID))
	set("x-yun-svc-type", "1")
	set("x-SvcType", "1")
	set("x-yun-module-type", "100")
	set("x-yun-app-channel", webChannel)
	set("x-yun-channel-source", webChannel)
	set("x-huawei-channelSrc", webChannel)
	set("x-m4c-caller", "PC")
	set("x-m4c-src", "10002")
	set("x-inner-ntwk", "2")
	set("mcloud-route", "001")
	set("mcloud-version", webVersion)
	set("mcloud-channel", "1000101")
	set("mcloud-client", "10701")
	set("mcloud-sign", sign)
	set("INNER-HCY-ROUTER-HTTPS", "1")
	set("x-yun-api-version", apiVersion)
	return h, nil
}

// userDomainHeaders 对应 Python _user_domain_headers：登录与短信接口的请求头。
func userDomainHeaders(clearBody, phone string, now time.Time) (http.Header, error) {
	deviceID := "web-0000"
	if len(phone) >= 4 {
		deviceID = "web-" + phone[len(phone)-4:]
	}
	sign, err := SignedHeader(clearBody, now)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	set := func(k, v string) { h.Set(k, v) }
	set("Accept", "application/json")
	set("Accept-Language", "zh-CN")
	set("Connection", "keep-alive")
	set("Content-Type", "application/json;charset=UTF-8")
	set("hcy-cool-flag", "1")
	set("x-NationCode", "+86")
	set("CMS-DEVICE", "default")
	set("X-Deviceinfo", webDeviceInfo(deviceID))
	set("x-yun-net-type", "wifi")
	set("x-NetType", "wifi")
	set("x-yun-channel-source", webChannel)
	set("x-huawei-channelSrc", webChannel)
	set("x-yun-svc-type", "1")
	set("x-SvcType", "1")
	set("x-m4c-caller", "PC")
	set("x-m4c-src", "10002")
	set("x-inner-ntwk", "2")
	set("mcloud-route", "001")
	set("mcloud-version", webVersion)
	set("mcloud-channel", "1000101")
	set("mcloud-client", "10701")
	set("mcloud-sign", sign)
	set("INNER-HCY-ROUTER-HTTPS", "1")
	set("x-yun-app-channel", webChannel)
	set("x-yun-client-info", webClientInfo(deviceID))
	set("x-yun-api-version", "v1")
	return h, nil
}

// downloadHeaders 对应 transport.default_http_headers：下载请求的通用头。
func downloadHeaders() http.Header {
	h := http.Header{}
	h.Set("User-Agent", "okhttp/13.2.4")
	h.Set("Platform", "Android")
	return h
}
