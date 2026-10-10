// login.go TRAE 浏览器登录：构造登录 URL + 解析回调链接。
//
// 与 trae2api 的双端口回调不同，omnigate 采用「粘贴回调链接」离线路径（与
// runable 一致）：面板生成登录 URL 并在浏览器打开，用户登录后 TRAE 跳到
// auth_callback_url（本机地址，打不开没关系），把地址栏里的整条 URL 粘回面板，
// 服务端从中解析 refreshToken 并完成 ExchangeToken + GetUserInfo + 落盘。
package trae

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultCallbackURL is the redirect target TRAE sends the user back to. It is
// intentionally a localhost address the user's browser cannot reach: the point
// is that the tokens appear in the address bar so the user can copy the URL.
const DefaultCallbackURL = "http://127.0.0.1:18080/authorize"

// loginTraceIDHexLen is the hex length of login_trace_id (matching login.sh).
const loginTraceIDHexLen = 16

// MachineTraceID derives a stable login_trace_id (hex16) from machine+device.
func MachineTraceID(machineID, deviceID string) string {
	h := machineID + deviceID
	if len(h) >= loginTraceIDHexLen {
		return h[len(h)-loginTraceIDHexLen:]
	}
	return strings.Repeat("0", loginTraceIDHexLen-len(h)) + h
}

// BuildLoginURL builds the TRAE authorization URL (mirrors login.sh).
func BuildLoginURL(machineID, deviceID, callbackURL string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", ClientID)
	v.Set("redirect", "0")
	return ConsoleHost + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(MachineTraceID(machineID, deviceID)) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(IdeVersion) +
		"&x_app_type=stable"
}

// CallbackInfo is the parsed login callback (raw credentials, server-side only).
type CallbackInfo struct {
	RefreshToken string
	AccessToken  string // fallback when no refreshToken is present
	UID          string
	Nickname     string
	EnterpriseID string
	ExpiresAt    int64
}

// parseJSONParam decodes a URL-encoded JSON query parameter (with one extra
// unquote for tolerance, mirroring login.sh).
func parseJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	return fmt.Sprintf("%v", v)
}

func getInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

// ParseCallback parses a TRAE login callback URL:
//
//	http://127.0.0.1:18080/authorize?refreshToken=...&userInfo={...}&userJwt={...}
//
// refreshToken is preferred; userJwt.RefreshToken / userJwt.Token are fallbacks.
func ParseCallback(rawURL string) (*CallbackInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("empty callback url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse callback url: %w", err)
	}
	q := u.Query()
	info := &CallbackInfo{RefreshToken: q.Get("refreshToken")}

	userInfo := parseJSONParam(q.Get("userInfo"))
	info.UID = getString(userInfo, "UserID")
	info.Nickname = getString(userInfo, "ScreenName")
	info.EnterpriseID = getString(userInfo, "TenantID")

	userJwt := parseJSONParam(q.Get("userJwt"))
	if info.RefreshToken == "" {
		info.RefreshToken = getString(userJwt, "RefreshToken")
	}
	if info.RefreshToken == "" {
		info.AccessToken = getString(userJwt, "Token")
		if info.AccessToken == "" {
			return nil, fmt.Errorf("callback missing refreshToken and userJwt.Token")
		}
		if exp := getInt64(userJwt, "TokenExpireAt"); exp > 0 {
			info.ExpiresAt = normalizeExpiresAt(exp)
		}
	}
	return info, nil
}

// ExpireAtFromExchange normalizes ExchangeToken's expiry fields to unix seconds,
// preferring TokenExpireAt when it is still in the future.
func ExpireAtFromExchange(tokenExpireAt, tokenExpireDuration int64, now time.Time) int64 {
	if tokenExpireAt > 0 {
		if exp := normalizeExpiresAt(tokenExpireAt); exp > now.Unix() {
			return exp
		}
	}
	if tokenExpireDuration > 0 {
		return now.Add(time.Duration(tokenExpireDuration) * time.Second).Unix()
	}
	return 0
}
