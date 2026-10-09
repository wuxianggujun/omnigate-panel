package outbound

import (
	"net/http"
	"testing"
)

func TestNormalizeAndResolve(t *testing.T) {
	c := Config{
		Proxies: []Proxy{
			{Name: "cn", URL: "socks5://127.0.0.1:1080"},
			{Name: "auth", URL: "127.0.0.1:8080", Username: "u", Password: "p"},
		},
		Routes: map[string]string{
			TargetWorkbuddy:       "cn",
			OmniTarget("raccoon"): "auth",
			"omnigate:direct":     "",
		},
	}
	if err := c.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if _, ok := c.Routes["omnigate:direct"]; ok {
		t.Fatalf("空路由应被删除")
	}
	u := c.For(TargetWorkbuddy)
	if u == nil || u.Scheme != "socks5" || u.Host != "127.0.0.1:1080" {
		t.Fatalf("workbuddy 代理解析错误: %+v", u)
	}
	u = c.For(OmniTarget("raccoon"))
	if u == nil || u.Scheme != "http" || u.Host != "127.0.0.1:8080" {
		t.Fatalf("raccoon 代理应补 http scheme: %+v", u)
	}
	if u.User == nil || u.User.Username() != "u" {
		pw, _ := u.User.Password()
		t.Fatalf("认证合并错误: %+v pw=%q", u.User, pw)
	}
	if c.For("omnigate:unknown") != nil {
		t.Fatalf("未配置目标应直连")
	}
}

func TestNormalizeErrors(t *testing.T) {
	cases := []Config{
		{Proxies: []Proxy{{Name: "", URL: "http://x:1"}}},
		{Proxies: []Proxy{{Name: "a", URL: ""}}},
		{Proxies: []Proxy{{Name: "a", URL: "http://x:1"}, {Name: "a", URL: "http://y:2"}}},
		{Proxies: []Proxy{{Name: "a", URL: "ftp://x:1"}}},
		{Proxies: []Proxy{{Name: "a", URL: "http://x:1"}}, Routes: map[string]string{"workbuddy": "missing"}},
	}
	for i, c := range cases {
		if err := c.Normalize(); err == nil {
			t.Fatalf("case %d 应当报错", i)
		}
	}
}

func TestApply(t *testing.T) {
	c := Config{Proxies: []Proxy{{Name: "cn", URL: "http://127.0.0.1:8888"}}, Routes: map[string]string{TargetWorkbuddy: "cn"}}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{}
	c.Apply(tr, TargetWorkbuddy)
	if tr.Proxy == nil {
		t.Fatal("Proxy 未设置")
	}
	req, _ := http.NewRequest("GET", "https://copilot.tencent.com/x", nil)
	u, err := tr.Proxy(req)
	if err != nil || u == nil || u.Host != "127.0.0.1:8888" {
		t.Fatalf("Proxy 函数错误: %v %+v", err, u)
	}
	// 直连目标应清空
	c.Apply(tr, "omnigate:unknown")
	if tr.Proxy != nil {
		t.Fatal("直连目标应清空 Proxy")
	}
}
