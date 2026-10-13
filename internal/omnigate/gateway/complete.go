package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb"
)

// CompleteText runs a single non-streaming chat turn and returns the assistant's
// text. It is the building block for server-side multi-step flows (deep research)
// that need the model's answer as a value rather than an HTTP response. Routing,
// global model aliases, and Gem-as-model resolution behave exactly like
// HandleChat so a research step can target any configured model.
func (g *Gateway) CompleteText(ctx context.Context, model, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("prompt 不能为空")
	}
	provName, m := g.route(model)
	prov := g.providers[provName]
	if prov == nil {
		return "", fmt.Errorf("未知 provider: %s", provName)
	}
	upstreamModel := g.resolveModel(ctx, provName, m)

	// Same Gem-as-model resolution as HandleChat: a mapped model name routes to
	// the Gem's base model and pins the owning account.
	var gemID, gemAccount string
	if gp, ok := prov.(*geminiweb.Provider); ok {
		if gm, ok := gp.ResolveGem(m); ok {
			gemID = gm.GemID
			gemAccount = gm.AccountID
			if gm.BaseModel != "" {
				upstreamModel = gm.BaseModel
			}
		}
	}

	req := &openai.ChatRequest{
		Model:    model,
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: prompt}}},
	}
	in := g.buildInput(prov, upstreamModel, req, g.cfg.Mode == "incognito", false)
	in.GemID = gemID
	in.PreferredAccount = gemAccount

	res, err := g.runChat(ctx, provName, prov, in, false, nil, nil)
	if err != nil {
		return "", err
	}
	return res.text, nil
}
