package gateway

import (
	"context"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// TestListModelsHonorsWhitelist 全局模型白名单（MODEL_WHITELIST）非空时，模型目录
// 只该列出白名单内的对外模型 ID；为空则放行全部（移植自参考实现 app/config.py 的
// model_whitelist 与 app/routers/{openai,claude,gemini}.py 的 _apply_model_whitelist）。
func TestListModelsHonorsWhitelist(t *testing.T) {
	g := catalogGateway(t,
		&catalogProvider{name: "raccoon", models: []openai.Model{{ID: "glm-5.3"}}},
		&catalogProvider{name: "runable", models: []openai.Model{{ID: "zai/glm-5.3-flash"}}},
	)
	ctx := context.Background()

	// 未设白名单：全量。
	if got := modelIDs(g.ListModels(ctx)); len(got) != 2 {
		t.Fatalf("未设白名单应是全量，得到 %v", got)
	}

	// 白名单只放行 raccoon/glm-5.3。
	g.cfg.ModelWhitelist = []string{"raccoon/glm-5.3"}
	got := modelIDs(g.ListModels(ctx))
	if len(got) != 1 || got[0] != "raccoon/glm-5.3" {
		t.Fatalf("白名单过滤得到 %v want [raccoon/glm-5.3]", got)
	}

	// 白名单里的名字都不存在：目录为空，绝不回落全量。
	g.cfg.ModelWhitelist = []string{"nope/none"}
	if got := modelIDs(g.ListModels(ctx)); len(got) != 0 {
		t.Fatalf("全部被挡应得到空目录，得到 %v", got)
	}
}
