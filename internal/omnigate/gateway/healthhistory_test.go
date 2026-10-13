package gateway

import (
	"context"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb"
)

// Probing all accounts must feed the provider's health ledger, and
// GeminiWebHealthHistory must aggregate it back (gemini2api /admin/check-account
// + /admin/health-history).
func TestGeminiWebHealthHistoryAggregates(t *testing.T) {
	g := testGateway(&scriptProvider{})
	g.cfg.Providers = []config.ProviderConfig{{Name: "gw", Type: "geminiweb"}}
	gp := geminiweb.NewProvider("gw", nil)
	g.providers["gw"] = gp
	g.order = []string{"gw"}
	g.accounts["gw"] = []*provider.Account{{Label: "a"}} // no cookie → probe fails offline

	res, err := g.GeminiWebCheckAccounts(context.Background(), "gw")
	if err != nil {
		t.Fatalf("check accounts: %v", err)
	}
	if len(res) != 1 || res[0].Success {
		t.Fatalf("unexpected check results: %+v", res)
	}

	total, records, err := g.GeminiWebHealthHistory("gw")
	if err != nil {
		t.Fatalf("health history: %v", err)
	}
	if total != 1 || len(records) != 1 {
		t.Fatalf("unexpected history size: total=%d records=%+v", total, records)
	}
	if records[0].Label != "a" || records[0].Valid || records[0].Error == "" {
		t.Fatalf("unexpected record: %+v", records[0])
	}
}
