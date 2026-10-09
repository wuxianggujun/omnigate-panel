package panel

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/pool"
)

// wbAuth 一份原生（嵌套形）WorkBuddy auth 文件内容，realm=cn。
const wbAuth = `{"auth":{"accessToken":"at-1","refreshToken":"rt-1","expiresAt":1893456000,"domain":"","realm":"cn"},"account":{"uid":"1001","nickname":"wb-1001"}}`

func writeAuthFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// multipartBody 构造一个含单文件的 multipart 请求体与 Content-Type。
func multipartBody(t *testing.T, field, filename, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

// 导出：auths 目录里的原生 auth 文件应原样出现在 workbuddy 段，omnigate 段来自注入闭包，
// 且带附件下载头。
func TestExportAccounts(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, filepath.Join(dir, "workbuddy-1001.json"), wbAuth)

	omniCalls := 0
	p := New(Config{
		Version: "test", APIKey: "k", Pool: pool.New(""), AuthDir: dir,
		ExportOmniAccounts: func() (json.RawMessage, error) {
			omniCalls++
			return json.RawMessage(`{"raccoon":[{"label":"a1"}]}`), nil
		},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/accounts/export", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var b struct {
		Version   int               `json:"version"`
		WorkBuddy []json.RawMessage `json:"workbuddy"`
		OmniGate  json.RawMessage   `json:"omnigate"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	if b.Version != 1 {
		t.Errorf("version=%d want 1", b.Version)
	}
	if len(b.WorkBuddy) != 1 {
		t.Fatalf("workbuddy len=%d want 1", len(b.WorkBuddy))
	}
	if !strings.Contains(string(b.OmniGate), "raccoon") {
		t.Errorf("omnigate=%s", b.OmniGate)
	}
	if omniCalls != 1 {
		t.Errorf("ExportOmniAccounts calls=%d want 1", omniCalls)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition=%q want attachment", cd)
	}
}

// 导入（multipart）：workbuddy 落盘进池 + omnigate 原始 JSON 交给注入闭包。
func TestImportAccountsMultipart(t *testing.T) {
	dir := t.TempDir()
	var gotOmni json.RawMessage
	p := New(Config{
		Version: "test", APIKey: "k", Pool: pool.New(""), AuthDir: dir,
		ImportOmniAccounts: func(raw json.RawMessage) (int, int, error) {
			gotOmni = raw
			return 1, 0, nil
		},
	})

	bundle := `{"version":1,"workbuddy":[` + wbAuth + `],"omnigate":{"raccoon":[{"label":"a1"}]}}`
	body, ct := multipartBody(t, "file", "bundle.json", bundle)
	req := httptest.NewRequest("POST", "/panel/api/accounts/import", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(dir, "workbuddy-1001.json")); err != nil {
		t.Fatalf("auth file not written: %v", err)
	}
	if a := p.cfg.Pool.AuthByUID("1001"); a == nil {
		t.Errorf("uid 1001 not added to pool")
	}
	if gotOmni == nil || !strings.Contains(string(gotOmni), "raccoon") {
		t.Errorf("omnigate raw not passed through: %s", gotOmni)
	}

	var resp struct {
		WorkBuddy struct {
			Imported int `json:"imported"`
			Skipped  int `json:"skipped"`
		} `json:"workbuddy"`
		OmniGate struct {
			Imported int `json:"imported"`
		} `json:"omnigate"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.WorkBuddy.Imported != 1 || resp.WorkBuddy.Skipped != 0 {
		t.Errorf("workbuddy counts=%+v", resp.WorkBuddy)
	}
	if resp.OmniGate.Imported != 1 {
		t.Errorf("omnigate imported=%d want 1", resp.OmniGate.Imported)
	}
}

// 导入（裸 JSON body）：顶层为数组时仅导入 WorkBuddy，且无 omnigate 闭包也不报错。
func TestImportAccountsBareArray(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), AuthDir: dir})

	body := "[" + wbAuth + "]"
	req := httptest.NewRequest("POST", "/panel/api/accounts/import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if a := p.cfg.Pool.AuthByUID("1001"); a == nil {
		t.Errorf("uid 1001 not added to pool")
	}
}

// 未鉴权导入/导出一律 401（包里含凭证，必须与其它 /panel/api/* 同口径）。
func TestAccountsIOUnauthorized(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), AuthDir: t.TempDir()})
	for _, c := range []struct{ method, path string }{
		{"GET", "/panel/api/accounts/export"},
		{"POST", "/panel/api/accounts/import"},
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != 401 {
			t.Errorf("%s %s status=%d want 401", c.method, c.path, rec.Code)
		}
	}
}
