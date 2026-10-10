package runable

import "testing"

// TestRunableModelCredits 校验 runable 上游条目的 credits / pricing / isFree
// 被映射到 openai.Model。
func TestRunableModelCredits(t *testing.T) {
	m := modelInfo{
		ID:                      "zai/glm-5.3-flash",
		IsFree:                  true,
		Credits:                 500,
		CreditsPerMillionTokens: true,
		Pricing:                 &modelPricing{Input: 1.5e-07, Output: 5e-07, InputCacheRead: 3e-08},
		ContextWindow:           1000000,
		MaxTokens:               131072,
	}
	got := runableModel(m)
	if got.Credits == nil || *got.Credits != 500 {
		t.Fatalf("credits=%v want 500", got.Credits)
	}
	if got.CreditUnit != "credits_per_million_tokens" {
		t.Fatalf("credit_unit=%q", got.CreditUnit)
	}
	if !got.IsFree {
		t.Fatalf("is_free=false want true")
	}
	if got.Pricing == nil || got.Pricing.Output != 5e-07 || got.Pricing.InputCacheRead != 3e-08 {
		t.Fatalf("pricing=%+v", got.Pricing)
	}
	if got.ContextWindow != 1000000 || got.MaxTokens != 131072 {
		t.Fatalf("sizes=%d/%d", got.ContextWindow, got.MaxTokens)
	}
}

// TestRunableModelNoCredits 无积分价时不编造，且 owned_by 兜底为 runable。
func TestRunableModelNoCredits(t *testing.T) {
	got := runableModel(modelInfo{ID: "openai/gpt-6-luna"})
	if got.Credits != nil || got.Pricing != nil || got.IsFree {
		t.Fatalf("expected no price, got %+v", got)
	}
	if got.OwnedBy != "runable" {
		t.Fatalf("owned_by=%q want runable", got.OwnedBy)
	}
}
