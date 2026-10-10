package gateway

import (
	"reflect"
	"testing"
)

// TestRunableCookieCandidates 裸值优先补 better-auth 的真实 Cookie 名（否则
// 「粘贴值」这条最常用的路径会被服务端判为未登录）；完整 Cookie（含 "="）原样返回。
func TestRunableCookieCandidates(t *testing.T) {
	got := runableCookieCandidates("ABC123")
	want := []string{
		"__Secure-better-auth.session_token=ABC123",
		"better-auth.session_token=ABC123",
		"session_token=ABC123",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bare value: got %v want %v", got, want)
	}

	full := "__Secure-better-auth.session_token=ABC123"
	if got := runableCookieCandidates(full); !reflect.DeepEqual(got, []string{full}) {
		t.Errorf("full cookie: got %v want [%s]", got, full)
	}

	if got := runableCookieCandidates("   "); got != nil {
		t.Errorf("blank: got %v want nil", got)
	}
}
