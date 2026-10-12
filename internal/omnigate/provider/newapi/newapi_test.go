package newapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
