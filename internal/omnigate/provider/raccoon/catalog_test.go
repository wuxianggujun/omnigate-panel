package raccoon

import (
	"context"
	"testing"
)

// TestParseCatalogBilling 校验上游 catalog 的 billing_* 字段被映射到 openai.Model：
// 生效倍率优先、单位标为 multiplier、原始元数据（含折扣）整体保留。
func TestParseCatalogBilling(t *testing.T) {
	payload := map[string]any{
		"data": map[string]any{
			"categories": []any{
				map[string]any{
					"models": []any{
						map[string]any{
							"name":                         "raccoon-8c4485",
							"params":                       map[string]any{"context_window": float64(1000000), "max_tokens": float64(100000)},
							"billing_multiplier":           float64(1),
							"billing_effective_multiplier": float64(0.5),
							"billing_category":             "normal",
							"billing_status":               "normal",
							"billing_status_note":          "",
							"billing_discounts": []any{
								map[string]any{"label": "夜间五折", "factor": float64(0.5)},
							},
						},
					},
				},
			},
		},
	}
	models := parseCatalog(payload)
	if len(models) != 1 {
		t.Fatalf("len=%d want 1", len(models))
	}
	m := models[0]
	if m.ID != "raccoon-8c4485" || m.ContextWindow != 1000000 || m.MaxTokens != 100000 {
		t.Fatalf("base fields = %+v", m)
	}
	if m.Credits == nil || *m.Credits != 0.5 {
		t.Fatalf("credits=%v want 0.5 (生效倍率优先)", m.Credits)
	}
	if m.CreditUnit != "multiplier" {
		t.Fatalf("credit_unit=%q want multiplier", m.CreditUnit)
	}
	if m.Billing == nil || m.Billing.Multiplier != 1 || m.Billing.Effective != 0.5 || m.Billing.Category != "normal" {
		t.Fatalf("billing=%+v", m.Billing)
	}
	if len(m.Billing.Discounts) != 1 {
		t.Fatalf("discounts=%+v", m.Billing.Discounts)
	}
}

// TestParseCatalogNoBilling 无计费字段时不应编造：Credits/Billing 均为 nil。
func TestParseCatalogNoBilling(t *testing.T) {
	payload := map[string]any{
		"categories": []any{
			map[string]any{
				"models": []any{
					map[string]any{"name": "plain-model", "params": map[string]any{"context_window": float64(8192)}},
				},
			},
		},
	}
	models := parseCatalog(payload)
	if len(models) != 1 {
		t.Fatalf("len=%d want 1", len(models))
	}
	if models[0].Credits != nil || models[0].Billing != nil {
		t.Fatalf("expected no price, got credits=%v billing=%+v", models[0].Credits, models[0].Billing)
	}
}

// TestListModelsWithAccountFallback 无账号（或空凭证）时退回内置默认目录，
// 且默认目录不带积分价（不编造）。
func TestListModelsWithAccountFallback(t *testing.T) {
	p := NewProvider("raccoon", "https://example.invalid", "", "")
	models, err := p.ListModelsWithAccount(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 {
		t.Fatal("expected fallback models")
	}
	for _, m := range models {
		if m.Credits != nil || m.Billing != nil {
			t.Fatalf("fallback model %s should have no price", m.ID)
		}
	}
}
