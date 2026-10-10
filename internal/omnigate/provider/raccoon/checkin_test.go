package raccoon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// checkinServer 桩：grant 返回 granted=false，bills 返回给定条目。
func checkinServer(t *testing.T, bills string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/login/points/grant"):
			io.WriteString(w, `{"code":0,"data":{"granted":false}}`)
		case strings.HasSuffix(r.URL.Path, "/points/v1/bills"):
			io.WriteString(w, bills)
		default:
			http.NotFound(w, r)
		}
	}))
}

// 今天没有新入账、也没有出错 → AlreadyClaimed（面板给中性提示，而不是红字「失败」）。
func TestCheckinAlreadyClaimed(t *testing.T) {
	srv := checkinServer(t, `{"code":0,"data":{"items":[]}}`)
	defer srv.Close()

	res, err := NewClient(srv.URL, "", "").Checkin(context.Background(), "tok")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.Success {
		t.Fatalf("Success = true, want false: %+v", res)
	}
	if !res.AlreadyClaimed {
		t.Fatalf("AlreadyClaimed = false, want true: %+v", res)
	}
}

// 今天确有积分入账 → Success，且不算「已领」。
func TestCheckinGrantedToday(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	bills := `{"code":0,"data":{"items":[{"points":300,"created_at":"` + today + `T10:00:00Z","event_name":"每日积分发放"}]}}`
	srv := checkinServer(t, bills)
	defer srv.Close()

	res, err := NewClient(srv.URL, "", "").Checkin(context.Background(), "tok")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.Success {
		t.Fatalf("Success = false, want true: %+v", res)
	}
	if res.AlreadyClaimed {
		t.Fatalf("AlreadyClaimed = true, want false: %+v", res)
	}
}

// 账单接口出错（无法核对）→ 既不算成功也不算「已领」，避免把故障说成「今天领过了」。
func TestCheckinBillsErrorNotAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/login/points/grant"):
			io.WriteString(w, `{"code":0,"data":{"granted":false}}`)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	res, err := NewClient(srv.URL, "", "").Checkin(context.Background(), "tok")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.Success || res.AlreadyClaimed {
		t.Fatalf("res = %+v, want neither success nor already-claimed", res)
	}
	if res.GrantsError == "" {
		t.Fatalf("GrantsError empty, want set: %+v", res)
	}
}
