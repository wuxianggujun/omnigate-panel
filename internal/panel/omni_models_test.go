package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestOmniModelsCarriesCatalogIssues 钉住「模型目录少了几家」必须能看见。
//
// 网关拉不到某家上游的目录时，过去只是把模型从列表里去掉：面板看不出「这家
// 没读到」和「这家没有模型」的区别。现在 /panel/api/models 附带 issues，
// 前端据此写「谁的目录没读到、是否还剩缓存」。
func TestOmniModelsCarriesCatalogIssues(t *testing.T) {
	issue := map[string]any{
		"provider": "aotera",
		"at":       int64(1793110000),
		"cached":   false,
		"error":    "upstream HTTP 401: invalid token for user@example.com",
	}
	p, _ := mustPanel(t, Config{
		OmniModels: func(context.Context) (any, error) {
			return []any{map[string]any{"id": "atria/glm-5.2", "object": "model"}}, nil
		},
		OmniModelIssues: func(includeError bool) any {
			row := map[string]any{
				"provider": issue["provider"],
				"at":       issue["at"],
				"cached":   issue["cached"],
			}
			if includeError {
				row["error"] = issue["error"]
			}
			return []map[string]any{row}
		},
	})

	read := func(cookie *http.Cookie) map[string]any {
		t.Helper()
		rec := do(p, http.MethodGet, "/panel/api/models", "", cookie, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /panel/api/models = %d body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应非法 JSON: %v", err)
		}
		return body
	}

	// 匿名：能知道「aotera 没读到」，但拿不到上游错误原文（端点匿名可读，
	// 上游报错里可能带回账号信息或密钥片段）。
	anon := read(nil)
	issues, ok := anon["issues"].([]any)
	if !ok || len(issues) != 1 {
		t.Fatalf("匿名 issues = %#v，期望 1 条", anon["issues"])
	}
	row := issues[0].(map[string]any)
	if row["provider"] != "aotera" || row["cached"] != false {
		t.Fatalf("匿名 issues[0] = %#v", row)
	}
	if _, leaked := row["error"]; leaked {
		t.Fatalf("匿名响应不该带上游错误原文: %#v", row)
	}

	// 已登录：保留原文，便于排查是哪家上游、什么原因。
	authed := read(login(t, p, "admin", "pw123456"))
	issues = authed["issues"].([]any)
	row = issues[0].(map[string]any)
	if row["error"] != issue["error"] {
		t.Fatalf("登录用户 issues[0].error = %#v, want %q", row["error"], issue["error"])
	}
}

// TestOmniModelsWithoutIssuesCallback 精简部署（未装配回调）时不写 issues 字段：
// 前端据此区分「没有已知问题」与「这个版本不支持」。
func TestOmniModelsWithoutIssuesCallback(t *testing.T) {
	p, _ := mustPanel(t, Config{
		OmniModels: func(context.Context) (any, error) { return []any{}, nil },
	})
	rec := do(p, http.MethodGet, "/panel/api/models", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /panel/api/models = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应非法 JSON: %v", err)
	}
	if _, present := body["issues"]; present {
		t.Fatalf("未装配回调时不该有 issues 字段: %#v", body["issues"])
	}
}
