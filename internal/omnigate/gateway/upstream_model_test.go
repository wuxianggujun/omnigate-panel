package gateway

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// TestReqMetaRecordsUpstreamModel 钉住：请求记录必须区分「客户端请求的名字」与
// 「真正发给上游的模型」。
//
// 起因是一次真实排障：别名 deepseek-v4.1-flash → cline-pass/cline-pass/…-1017
// 被上游预改写成带前缀的 ID 后整条别名链失效，但 request_logs.model 记的是客户端
// 那个名字，两条路径的日志一模一样，从记录上完全看不出别名有没有生效。没有
// UpstreamModel 这一列，这类问题只能靠翻网关 stdout 反推。
func TestReqMetaRecordsUpstreamModel(t *testing.T) {
	g := testGateway(&scriptProvider{})
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
	// 别名指向一个「目录里没有该模型」的目标：resolveModel 无法补全时按字面返回，
	// 正好用来验证回填的是解析后的目标而不是别名本身。
	if err := g.modelMap.Set("alias-model", "fake/real-upstream"); err != nil {
		t.Fatal(err)
	}

	meta := &ReqMeta{}
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), meta), &openai.ChatRequest{
		Model:    "alias-model",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}, w)

	if meta.Model != "alias-model" {
		t.Fatalf("Model = %q, want alias-model (客户端请求名)", meta.Model)
	}
	if meta.UpstreamModel != "real-upstream" {
		t.Fatalf("UpstreamModel = %q, want real-upstream (别名解析后的真实目标)", meta.UpstreamModel)
	}
}

// TestReqMetaUpstreamModelEqualsRequestForPlainModel 钉住反例：没有别名时两者相同，
// 前端据此才敢「相同就不显示第二行」。
func TestReqMetaUpstreamModelEqualsRequestForPlainModel(t *testing.T) {
	g := testGateway(&scriptProvider{})
	meta := &ReqMeta{}
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), meta), &openai.ChatRequest{
		Model:    "fake/m",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}, w)

	if meta.Model != "fake/m" || meta.UpstreamModel != "m" {
		t.Fatalf("Model=%q UpstreamModel=%q, want fake/m and m", meta.Model, meta.UpstreamModel)
	}
}
