package newapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

	res, err := NewClient(srv.URL).Checkin(context.Background(), "tok")
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

	b, err := NewClient(srv.URL).Refresh(context.Background(), "sid1", "")
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
	if _, err := NewClient("http://127.0.0.1:1").Refresh(context.Background(), "", ""); err == nil {
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

	s, err := NewClient(srv.URL).Self(context.Background(), "tok")
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
	if _, err := c.Self(context.Background(), "tok"); err != nil {
		t.Fatalf("self: %v", err)
	}
	if got.ua != provider.BrowserUA {
		t.Errorf("User-Agent=%q", got.ua)
	}
	if got.al != provider.AcceptLanguage {
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

	if _, err := c.Checkin(context.Background(), "tok"); err != nil {
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
