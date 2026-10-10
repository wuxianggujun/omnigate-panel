// secret.go 面板凭证脱敏：所有会把凭证回显给浏览器的接口（供应商配置 / 出站代理 /
// 主配置）一律只给「前缀 + 长度」掩码，原始值绝不离开服务端；保存时按「掩码 =
// 未改动」从落盘配置还原，保证前端整份回传（含掩码）也不会把密钥写成掩码串。
//
// 掩码形态：前 6 字符 + "…(len=N)"，如 sk-6d06…(len=48)（len 按字节计）。
// 判据：值里含 "…(len=" 即视为掩码——真实凭证不含该子串，误判概率可忽略。
//
// 为什么要这一层：面板是多人共用的（admin 全权 / viewer 只读），而「供应商配置」
// 是**只读接口**（viewer 也能 GET）。此前 GET /panel/api/omni/config 会把
// omnigate.json 原样吐给前端，里面含各 provider 账号的 cookie/password/refresh_token
// 与上游 api_key——等于任何人登录面板都能抄走全部账号。
//
// 例外（刻意不脱敏）：顶层 api_key / api_keys 是**网关自己的**密钥，面板前端要用它
// 调 /omni/*（见 app.js 的 LS_KEY 缓存与 apiAbs），脱敏会让整个 OmniGate 面板不可用。
package panel

import (
	"encoding/json"
	"strconv"
	"strings"
)

// maskMarker 掩码特征子串：值里含它即视为「已脱敏」。
const maskMarker = "…(len="

// secretKey 字段名是否为凭证（大小写不敏感）：命中则整值替换为掩码。
// 覆盖 config.json / omnigate.json 两个视图里的全部凭证键。
func secretKey(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "cookie", "password", "passwd", "refresh_token", "access_token",
		"token", "api_key", "apikey", "secret", "client_secret",
		"session_token", "device_token", "authorization", "credential", "credentials":
		return true
	}
	return false
}

// urlKey 字段名是否为 URL：只把 userinfo（user:pass@）脱敏，保留 scheme/host/port
// 便于运维辨认（整值脱敏会让人无法核对代理地址）。
func urlKey(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "url", "pool_url", "proxy", "proxy_url", "base_url", "chat_base", "billing_base":
		return true
	}
	return false
}

// isMasked 值是否已是掩码形态。
func isMasked(s string) bool { return strings.Contains(s, maskMarker) }

// maskSecret 保留前 6 字符 + 长度标记；短于 12 字符时只露前一半（否则 5 字符的密码
// 会被整段显示，等于没脱敏）；空串与已脱敏值原样返回。
func maskSecret(s string) string {
	if s == "" || isMasked(s) {
		return s
	}
	n := len(s)
	head := 6
	if n < 12 {
		head = n / 2
	}
	return s[:head] + maskMarker + strconv.Itoa(n) + ")"
}

// maskURLUserinfo 把 URL 里的 user:pass@ 换成掩码，保留其余部分。无 userinfo 时
// 原样返回（绝大多数代理 URL 无凭据，此函数等于空操作）。
func maskURLUserinfo(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	rest := raw[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return raw
	}
	// "@" 出现在路径/查询里就不是 userinfo（如 http://host/a@b）。
	if j := strings.IndexAny(rest, "/?#"); j >= 0 && j < at {
		return raw
	}
	return raw[:i+3] + maskSecret(rest[:at]) + rest[at:]
}

// toGeneric 把任意结构体/指针转成 map[string]any / []any 树（JSON 往返）。
// 失败返回错误——调用方必须据此拒绝回显，绝不能退化成「原样返回」。
func toGeneric(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// maskSecrets 递归脱敏任意 JSON 值里的凭证字段（返回副本，不修改入参）。
// skipTop 里的键名在**顶层**跳过（如网关 api_key / api_keys）。
func maskSecrets(v any, skipTop ...string) any {
	skip := make(map[string]bool, len(skipTop))
	for _, k := range skipTop {
		skip[strings.ToLower(k)] = true
	}
	return maskWalk(v, skip, true)
}

func maskWalk(v any, skipTop map[string]bool, top bool) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if top && skipTop[strings.ToLower(k)] {
				out[k] = val
				continue
			}
			if s, ok := val.(string); ok && s != "" {
				// 含 "{{...}}" 的是模板占位（如签到任务 body 里的 {{cookie}}），
				// 不是真凭证：脱敏会让人看不懂，且它本就不含密钥。
				if secretKey(k) && !strings.Contains(s, "{{") {
					out[k] = maskSecret(s)
					continue
				}
				if urlKey(k) {
					out[k] = maskURLUserinfo(s)
					continue
				}
			}
			out[k] = maskWalk(val, skipTop, false)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = maskWalk(val, skipTop, false)
		}
		return out
	default:
		return v
	}
}

// restoreSecrets 保存前还原未改动的掩码值：新值仍是掩码 → 用旧配置里的原值替换。
// 数组元素按身份键（name/label/id/uid）匹配旧元素，匹配不到再退回同下标；这样
// 前端整份回传也不会把密钥写成掩码串。
func restoreSecrets(newV, oldV any) any {
	switch nt := newV.(type) {
	case map[string]any:
		old, _ := oldV.(map[string]any)
		out := make(map[string]any, len(nt))
		for k, val := range nt {
			var o any
			if old != nil {
				o = old[k]
			}
			out[k] = restoreSecrets(val, o)
		}
		return out
	case []any:
		old, _ := oldV.([]any)
		out := make([]any, len(nt))
		for i, val := range nt {
			out[i] = restoreSecrets(val, matchOldElement(val, old, i))
		}
		return out
	case string:
		if !isMasked(nt) {
			return nt
		}
		old, ok := oldV.(string)
		if !ok || old == "" || isMasked(old) {
			// 没有可还原的原值（新增字段/旧值也缺失）：原样保留掩码，交由上层校验报错，
			// 绝不猜测密钥。
			return nt
		}
		if r, ok := restoreURLUserinfo(nt, old); ok {
			return r
		}
		return old
	default:
		return newV
	}
}

// identityOf 取数组元素的身份键（"name=x" / "label=y"），无身份键返回空串。
func identityOf(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"name", "label", "id", "uid"} {
		if s, ok := m[k].(string); ok && s != "" {
			return k + "=" + s
		}
	}
	return ""
}

// matchOldElement 为「前端回传的第 i 个元素」找旧配置里对应的元素：身份键一致才用
// 同下标（防止「删掉第 0 个」导致张冠李戴地把别人的密钥还原进来），否则按身份键全表查。
func matchOldElement(newEl any, old []any, i int) any {
	id := identityOf(newEl)
	if i < len(old) && identityOf(old[i]) == id {
		return old[i]
	}
	if id == "" {
		return nil
	}
	for _, o := range old {
		if identityOf(o) == id {
			return o
		}
	}
	return nil
}

// restoreURLUserinfo 新值是「URL 里 userinfo 被掩码」的形态时，只把 userinfo 段换回
// 旧值，保留用户对 host/port/path 的改动；不适用则返回 false（调用方整值回退）。
func restoreURLUserinfo(newURL, oldURL string) (string, bool) {
	ni := strings.Index(newURL, "://")
	oi := strings.Index(oldURL, "://")
	if ni < 0 || oi < 0 {
		return "", false
	}
	nrest, orest := newURL[ni+3:], oldURL[oi+3:]
	nat, oat := strings.Index(nrest, "@"), strings.Index(orest, "@")
	if nat < 0 || oat < 0 {
		return "", false
	}
	if strings.IndexAny(nrest[:nat], "/?#") >= 0 || !isMasked(nrest[:nat]) {
		return "", false
	}
	return newURL[:ni+3] + orest[:oat] + nrest[nat:], true
}

// restoreMaskedSecrets 把 incoming（前端整份回传，凭证字段可能是掩码）里的掩码值用
// loadOld() 读到的落盘配置还原。读旧配置失败时原样返回——让上层校验去拦，不静默改数据。
func restoreMaskedSecrets(incoming any, loadOld func() (any, error)) any {
	old, err := loadOld()
	if err != nil || old == nil {
		return incoming
	}
	oldGen, err := toGeneric(old)
	if err != nil {
		return incoming
	}
	return restoreSecrets(incoming, oldGen)
}
