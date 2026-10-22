package newapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/egress"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

func TestStripBearer(t *testing.T) {
	cases := map[string]string{
		"Bearer sk-abc":   "sk-abc",
		"bearer sk-abc":   "sk-abc",
		"  Bearer  sk-x ": "sk-x",
		"sk-plain":        "sk-plain",
		"":                "",
	}
	for in, want := range cases {
		if got := stripBearer(in); got != want {
			t.Errorf("stripBearer(%q)=%q want %q", in, got, want)
		}
	}
}

func TestIsAlreadyClaimed(t *testing.T) {
	for _, s := range []string{"今日已签到", "已经签到过了", "Already checked in", "请勿重复签到"} {
		if !isAlreadyClaimed(s) {
			t.Errorf("%q 应判为「已签到」", s)
		}
	}
	if isAlreadyClaimed("签到成功") {
		t.Error("「签到成功」不该判为已签到")
	}
}

func TestCheckinAlreadyClaimedIsNeutral(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/checkin" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"message":"今日已签到"}`))
	}))
	defer srv.Close()

	res, err := NewClient(srv.URL).Checkin(context.Background(), &provider.Account{Cookie: "tok"})
	if err != nil {
		t.Fatalf("已签到不该返回 error：%v", err)
	}
	if !res.AlreadyClaimed {
		t.Fatalf("应标记 AlreadyClaimed，得到 %+v", res)
	}
}

func TestRefreshSendsSessionHeader(t *testing.T) {
	var gotSID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSID = r.Header.Get("X-Auth-Session")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"newtok","token_type":"Bearer","access_expires_at":1999999999,"session":{"sid":"sid2"},"user":{"id":7,"username":"u7"}}}`))
	}))
	defer srv.Close()

	b, err := NewClient(srv.URL).Refresh(context.Background(), "sid1", "", "")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if gotSID != "sid1" {
		t.Fatalf("应带 X-Auth-Session=sid1，得到 %q", gotSID)
	}
	if b.AccessToken != "newtok" || b.SessionSID != "sid2" || b.UserID != 7 || b.ExpiresAt != 1999999999 {
		t.Fatalf("bundle 解析错误：%+v", b)
	}
}

func TestRefreshRequiresSession(t *testing.T) {
	if _, err := NewClient("http://127.0.0.1:1").Refresh(context.Background(), "", "", ""); err == nil {
		t.Fatal("缺少 session 应报错")
	}
}

func TestSelfParsesQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Fatalf("Authorization=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":9,"username":"nine","quota":12345,"used_quota":678,"group":"default"}}`))
	}))
	defer srv.Close()

	s, err := NewClient(srv.URL).Self(context.Background(), &provider.Account{Cookie: "tok"})
	if err != nil {
		t.Fatalf("self: %v", err)
	}
	if s.UserID != 9 || s.Quota != 12345 || s.UsedQuota != 678 || s.Username != "nine" {
		t.Fatalf("self 解析错误：%+v", s)
	}
}

func TestUnauthorized(t *testing.T) {
	if !Unauthorized(&UpstreamError{Status: 401}) {
		t.Error("401 应判为鉴权失败")
	}
	if Unauthorized(&UpstreamError{Status: 500}) {
		t.Error("500 不该判为鉴权失败")
	}
}

// TestBrowserHeaders 锁定出站指纹：每个请求都要像站点自己的同源 XHR，而不是
// 脚本（UA 真实、带 Accept-Language / Referer / Sec-Fetch-*，GET 不带
// Origin/Content-Type，POST 才带）。
func TestBrowserHeaders(t *testing.T) {
	type seen struct {
		ua, al, site, mode, dest, ref, origin, ctype string
	}
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header
		got = seen{
			ua:     h.Get("User-Agent"),
			al:     h.Get("Accept-Language"),
			site:   h.Get("Sec-Fetch-Site"),
			mode:   h.Get("Sec-Fetch-Mode"),
			dest:   h.Get("Sec-Fetch-Dest"),
			ref:    h.Get("Referer"),
			origin: h.Get("Origin"),
			ctype:  h.Get("Content-Type"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":1,"username":"u"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if _, err := c.Self(context.Background(), &provider.Account{Cookie: "tok"}); err != nil {
		t.Fatalf("self: %v", err)
	}
	if got.ua != egress.ChromeUA {
		t.Errorf("User-Agent=%q", got.ua)
	}
	if got.al != egress.AcceptLanguage {
		t.Errorf("Accept-Language=%q", got.al)
	}
	if got.site != "same-origin" || got.mode != "cors" || got.dest != "empty" {
		t.Errorf("Sec-Fetch-*=%q/%q/%q", got.site, got.mode, got.dest)
	}
	if got.ref != srv.URL+"/" {
		t.Errorf("Referer=%q", got.ref)
	}
	if got.origin != "" || got.ctype != "" {
		t.Errorf("GET 不该带 Origin/Content-Type，得到 %q/%q", got.origin, got.ctype)
	}

	if _, err := c.Checkin(context.Background(), &provider.Account{Cookie: "tok"}); err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if got.origin != srv.URL {
		t.Errorf("POST Origin=%q", got.origin)
	}
	if got.ctype != "" {
		t.Errorf("无 body 的 POST 不该带 Content-Type，得到 %q", got.ctype)
	}

	// 带 body 的 POST（聊天）要带 Content-Type。
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader("{}"))
	c.setBrowserHeaders(req, true, "text/event-stream")
	if req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("带 body 的 POST Content-Type=%q", req.Header.Get("Content-Type"))
	}
	if req.Header.Get("Origin") != srv.URL {
		t.Errorf("带 body 的 POST Origin=%q", req.Header.Get("Origin"))
	}
	if req.Header.Get("Accept") != "text/event-stream" {
		t.Errorf("Accept=%q", req.Header.Get("Accept"))
	}
}

// TestAnyRouterStyleSurface 覆盖 AnyRouter/AgentRouter 形态：sign_in 路径、
// New-Api-User 身份头、Cookie 会话鉴权（无 Authorization）。
func TestAnyRouterStyleSurface(t *testing.T) {
	var (
		gotPath, gotUser, gotCookie, gotAuth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser = r.Header.Get("New-Api-User")
		gotCookie = r.Header.Get("Cookie")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checkin_count":1}}}`))
	}))
	defer srv.Close()

	eps := Endpoints{CheckinPath: "/api/user/sign_in", UserHeader: "New-Api-User", AuthMode: AuthCookie}
	c := NewClientWithOptions(srv.URL, eps, egress.Profile{})
	acc := &provider.Account{SessionCookie: "session=abc123", UID: "42"}
	if _, err := c.Checkin(context.Background(), acc); err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if gotPath != "/api/user/sign_in" {
		t.Errorf("path=%q", gotPath)
	}
	if gotUser != "42" {
		t.Errorf("New-Api-User=%q", gotUser)
	}
	if gotCookie != "session=abc123" {
		t.Errorf("Cookie=%q", gotCookie)
	}
	if gotAuth != "" {
		t.Errorf("cookie 模式不该带 Authorization，得到 %q", gotAuth)
	}
}

// TestClientProfileOverride 证明 provider.client 能覆盖该类型的历史默认指纹。
func TestClientProfileOverride(t *testing.T) {
	var gotUA, gotApp string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotApp = r.Header.Get("x-app")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":1}}`))
	}))
	defer srv.Close()

	c := NewClientWithOptions(srv.URL, Endpoints{}, egress.Profile{Preset: egress.PresetClaudeCode})
	if _, err := c.Self(context.Background(), &provider.Account{Cookie: "tok"}); err != nil {
		t.Fatalf("self: %v", err)
	}
	if !strings.HasPrefix(gotUA, "claude-cli/") {
		t.Errorf("User-Agent=%q", gotUA)
	}
	if gotApp != "cli" {
		t.Errorf("x-app=%q", gotApp)
	}
}

// TestCookieAuthFallsBackToSessionSID：抓取里只拿到 session id（没有整条 Cookie）时，
// Cookie 型站点也要能用——session=<sid> 就是站点会话 Cookie 的实际形态。
func TestCookieAuthFallsBackToSessionSID(t *testing.T) {
	var gotCookie, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotUser = r.Header.Get("New-Api-User")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":42,"username":"u"}}`))
	}))
	defer srv.Close()

	c := NewClientWithOptions(srv.URL,
		Endpoints{AuthMode: AuthCookie, UserHeader: "New-Api-User"}, egress.Profile{})
	acc := &provider.Account{RefreshToken: "sid-xyz", UID: "42"}
	if _, err := c.Self(context.Background(), acc); err != nil {
		t.Fatalf("self: %v", err)
	}
	if gotCookie != "session=sid-xyz" {
		t.Errorf("Cookie=%q", gotCookie)
	}
	if gotUser != "42" {
		t.Errorf("New-Api-User=%q", gotUser)
	}
}

// TestRefreshIsNoOpForCookieAuth：Cookie 型站点没有 token 可轮换，Refresh 必须
// 明确拒绝，而不是发出一个注定失败的请求（也绝不能拿会话去「刷新」把用户登出）。
func TestRefreshIsNoOpForCookieAuth(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := NewClientWithOptions(srv.URL, Endpoints{AuthMode: AuthCookie}, egress.Profile{})
	_, err := c.Refresh(context.Background(), "", "session=abc", "7")
	if err == nil {
		t.Fatal("cookie 型站点不该报告刷新成功")
	}
	if called {
		t.Error("不该真的发请求")
	}

	p := NewProviderWithOptions(Options{Name: "ar", BaseURL: srv.URL, Endpoints: Endpoints{AuthMode: AuthCookie}})
	acc := &provider.Account{Label: "a", SessionCookie: "session=abc"}
	if err := p.RefreshAccount(context.Background(), acc); err != nil {
		t.Errorf("RefreshAccount 对 cookie 账号应是成功的空操作，得到 %v", err)
	}
}

// TestTokenExpiredAcceptsSessionCookie：Cookie 型账号没有 access_token，但只要会话
// Cookie 在就视为有效，否则面板会把可用账号全标成「授权失效」。
func TestTokenExpiredAcceptsSessionCookie(t *testing.T) {
	p := NewProviderWithOptions(Options{Name: "ar", BaseURL: "http://127.0.0.1:1", Endpoints: Endpoints{AuthMode: AuthCookie}})
	if p.TokenExpired(&provider.Account{SessionCookie: "session=abc"}) {
		t.Error("有会话 Cookie 时不该判为过期")
	}
	if !p.TokenExpired(&provider.Account{}) {
		t.Error("两种凭据都没有时应判为过期")
	}
}

// TestAnthropicProtocolChat：newapi 适配器在 protocol=anthropic 下走 /v1/messages，
// 用 x-api-key 鉴权，并解码 Anthropic SSE。
func TestAnthropicProtocolChat(t *testing.T) {
	var gotPath, gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	p := NewProviderWithOptions(Options{
		Name: "ar", BaseURL: srv.URL, Protocol: "anthropic",
		Client: egress.Profile{Preset: egress.PresetClaudeCode},
	})
	s, err := p.StreamChat(context.Background(), &provider.Account{APIKey: "sk-chat"}, provider.ChatInput{Model: "claude-sonnet-4"})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	defer s.Close()
	if gotPath != "/v1/messages" {
		t.Errorf("path=%q", gotPath)
	}
	if gotKey != "sk-chat" {
		t.Errorf("x-api-key=%q", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("Anthropic 方言不该带 Authorization，得到 %q", gotAuth)
	}
	var text string
	for {
		e, err := s.Recv()
		if err != nil {
			break
		}
		if e.Text != "" {
			text += e.Text
		}
	}
	if text != "你好" {
		t.Errorf("解码出的正文=%q", text)
	}
	if p.Protocol() != "anthropic" {
		t.Errorf("Protocol()=%q", p.Protocol())
	}
}

// TestMissingAPIKeyIsClearError：只建了会话、还没补 sk- 密钥的账号，报错必须说清
// 缺的是什么，而不是一个泛泛的上游错误。
func TestMissingAPIKeyIsClearError(t *testing.T) {
	p := NewProviderWithOptions(Options{Name: "ar", BaseURL: "http://127.0.0.1:1"})
	_, err := p.StreamChat(context.Background(), &provider.Account{SessionCookie: "session=abc"}, provider.ChatInput{})
	if err == nil {
		t.Fatal("没有 sk- 密钥时应报错")
	}
	if !strings.Contains(err.Error(), "API Key") {
		t.Errorf("错误信息应点明缺 API Key，得到 %q", err.Error())
	}
}
