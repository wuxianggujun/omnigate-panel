package gateway

import (
	"strings"
	"testing"
)

// 自动命名用的账号名清洗：去控制字符、折叠空白、限长。
func TestSanitizeLabel(t *testing.T) {
	long := strings.Repeat("x", 60)
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"alice", "alice"},
		{"  alice  ", "alice"},
		{"浣熊账号1", "浣熊账号1"},
		{"a\tb\nc", "a b c"},
		{"a\x00b", "ab"},
		{"a   b", "a b"},
		{long, strings.Repeat("x", 48)},
	}
	for _, c := range cases {
		if got := sanitizeLabel(c.in); got != c.want {
			t.Errorf("sanitizeLabel(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
