package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(authz string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func TestVerifyBearer(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		authz string
		want  bool
	}{
		{"空 key 放行（未启用鉴权）", "", "", true},
		{"空 key 也放行任意头", "", "Bearer whatever", true},
		{"正确 key", "sk-abc123", "Bearer sk-abc123", true},
		{"错误 key", "sk-abc123", "Bearer sk-wrong", false},
		{"缺 Authorization 头", "sk-abc123", "", false},
		{"缺 Bearer 前缀", "sk-abc123", "sk-abc123", false},
		{"前缀大小写不符（规范要求精确）", "sk-abc123", "bearer sk-abc123", false},
		{"多余空格", "sk-abc123", "Bearer  sk-abc123", false},
		{"前缀相同但内容短", "sk-abc123", "Bearer sk-abc12", false},
		{"前缀相同但内容长", "sk-abc123", "Bearer sk-abc1234", false},
		{"key 恰好是前缀", "sk-abc", "Bearer sk-abcdef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(req(c.authz), c.key); got != c.want {
				t.Errorf("VerifyBearer(key=%q, authz=%q) = %v, want %v", c.key, c.authz, got, c.want)
			}
		})
	}
}

// TestVerifyBearerWithoutHeaderStillCompares 缺头路径不应因"提前返回"而暴露形状差异：
// 这里只验证它确实返回 false 且不 panic（常量时间的性质无法用单测断言，靠实现保证）。
func TestVerifyBearerWithoutHeaderStillCompares(t *testing.T) {
	if VerifyBearer(req(""), "any-key") {
		t.Error("missing header must not pass")
	}
}

// TestVerifyBearerAPIKeyHeader 覆盖 Anthropic 客户端形态：Claude Code 走 x-api-key
// 而不是 Authorization: Bearer，两种头必须同口径放行；两个头同时存在时 Bearer 优先。
func TestVerifyBearerAPIKeyHeader(t *testing.T) {
	withAPIKey := func(v string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		if v != "" {
			r.Header.Set("x-api-key", v)
		}
		return r
	}
	cases := []struct {
		name string
		key  string
		req  *http.Request
		want bool
	}{
		{"x-api-key 正确", "sk-abc123", withAPIKey("sk-abc123"), true},
		{"x-api-key 错误", "sk-abc123", withAPIKey("sk-wrong"), false},
		{"x-api-key 为空", "sk-abc123", withAPIKey(""), false},
		{"空 key 放行任意 x-api-key", "", withAPIKey("whatever"), true},
		{"两个头都在时 Bearer 优先", "sk-abc123", func() *http.Request {
			r := withAPIKey("sk-abc123")
			r.Header.Set("Authorization", "Bearer sk-wrong")
			return r
		}(), false},
		{"Bearer 正确时忽略错的 x-api-key", "sk-abc123", func() *http.Request {
			r := withAPIKey("sk-wrong")
			r.Header.Set("Authorization", "Bearer sk-abc123")
			return r
		}(), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(c.req, c.key); got != c.want {
				t.Errorf("VerifyBearer(key=%q) = %v, want %v", c.key, got, c.want)
			}
		})
	}
}

func TestDigestIsFixedLength(t *testing.T) {
	// 不同长度输入摘要后应等长（这是常量时间比较的前提）
	if len(digest("")) != len(digest("a-much-longer-secret-value")) {
		t.Error("digest length must not depend on input length")
	}
	if len(digest("x")) != 32 {
		t.Errorf("sha256 digest length = %d, want 32", len(digest("x")))
	}
}
