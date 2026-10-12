// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// apiKeyHeader 是 Anthropic 客户端（Claude Code）使用的密钥头。Claude Code 走
// x-api-key 而不是 Authorization: Bearer——两种形态必须同口径放行，否则同一把密钥
// 在 /v1/messages 上会被判 401。
const apiKeyHeader = "x-api-key"

// VerifyBearer 校验请求头是否携带正确的密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 密钥来源两个头：Authorization: Bearer（OpenAI 系客户端）优先，其次 x-api-key
// （Anthropic 系客户端）。比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	tok := bearerToken(r)
	if tok == "" {
		// 缺头：仍走一次摘要比较，保持耗时形状一致。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// bearerToken 取请求携带的密钥原文：Authorization: Bearer 优先，其次 x-api-key。
// 两个头都缺时返回空串（调用方据此判定未通过）。
func bearerToken(r *http.Request) string {
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, bearerPrefix) {
		return authz[len(bearerPrefix):]
	}
	return r.Header.Get(apiKeyHeader)
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
