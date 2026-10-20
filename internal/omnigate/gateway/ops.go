package gateway

import (
	"context"
	"fmt"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
)

// ProviderOps is the gateway-side behavior set for one provider type. Callers
// (the manager / panel layer) resolve it by provider name and drive every
// account operation through this one interface, so they never switch on the
// type string.
//
// The set an adapter answers is bounded by the type's plugin descriptor: Supports
// reads the declared Actions, so a type that does not declare "checkin" is
// rejected before any upstream call.
type ProviderOps interface {
	// ListAccounts returns the provider's accounts as panel-ready rows. With
	// withBalance=false it returns the cheap in-memory view; withBalance=true may
	// hit the upstream for balances / session probes.
	ListAccounts(ctx context.Context, providerName string, withBalance bool) ([]map[string]any, error)
	// HandleAction executes one declared action. Account-scoped actions carry the
	// label in Params["label"] ("" = every account).
	HandleAction(ctx context.Context, providerName string, req plugin.ActionRequest) (plugin.ActionResult, error)
	// RemoveAccount drops a runtime account and its credentials.
	RemoveAccount(providerName, label string) bool
	// Supports reports whether the type declares the action.
	Supports(action plugin.ActionName) bool
}

// Ops resolves the behavior set for providerName. Unknown types fall back to
// genericOps (in-memory account view, no maintenance), matching a provider whose
// accounts are hand-written config.
func (g *Gateway) Ops(providerName string) ProviderOps {
	if adapter, ok := opsByType[g.ProviderType(providerName)]; ok {
		return adapter(g)
	}
	return genericOps{g}
}

// opsByType maps a plugin type to its gateway adapter. Adding a plugin type that
// needs account maintenance means adding one entry here; nothing else changes.
var opsByType = map[string]func(*Gateway) ProviderOps{
	"raccoon":   func(g *Gateway) ProviderOps { return raccoonOps{g} },
	"runable":   func(g *Gateway) ProviderOps { return runableOps{g} },
	"trae":      func(g *Gateway) ProviderOps { return traeOps{g} },
	"newapi":    func(g *Gateway) ProviderOps { return newapiOps{g} },
	"geminiweb": func(g *Gateway) ProviderOps { return geminiwebOps{g} },
	"opencode":  func(g *Gateway) ProviderOps { return opencodeOps{g} },
}

// declaresAction reads the type's descriptor; a missing descriptor means the
// action is unsupported (the type is not a registered plugin).
func declaresAction(typ string, action plugin.ActionName) bool {
	p, err := plugin.Get(typ)
	if err != nil {
		return false
	}
	return p.HasAction(action)
}

// genericAccountRows is the in-memory view every adapter falls back to when a
// live upstream query is not requested.
func (g *Gateway) genericAccountRows(providerName string) []map[string]any {
	out := []map[string]any{}
	for _, a := range g.Accounts(providerName) {
		out = append(out, map[string]any{"label": a.Label, "has_token": a.Cookie != ""})
	}
	return out
}

// actionLabel extracts the target account label from an action request.
func actionLabel(req plugin.ActionRequest) string {
	if req.Params == nil {
		return ""
	}
	return req.Params["label"]
}

// raccoonOutcomes / runableOutcomes / traeOutcomes / geminiOutcomes convert the
// provider-specific check-in result rows to the shared neutral shape the panel
// renders for every type.
func raccoonOutcomes(in []RaccoonCheckinOutcome) []plugin.AccountOutcome {
	out := make([]plugin.AccountOutcome, 0, len(in))
	for _, r := range in {
		out = append(out, plugin.AccountOutcome{
			Label: r.Label, Success: r.Success, Message: r.Message, Error: r.Error, AlreadyClaimed: r.AlreadyClaimed,
		})
	}
	return out
}

func runableOutcomes(in []RunableCheckinOutcome) []plugin.AccountOutcome {
	out := make([]plugin.AccountOutcome, 0, len(in))
	for _, r := range in {
		out = append(out, plugin.AccountOutcome{Label: r.Label, Success: r.Success, Message: r.Message, Error: r.Error})
	}
	return out
}

func traeOutcomes(in []TraeCheckinOutcome) []plugin.AccountOutcome {
	out := make([]plugin.AccountOutcome, 0, len(in))
	for _, r := range in {
		out = append(out, plugin.AccountOutcome{
			Label: r.Label, Success: r.Success, Message: r.Message, Error: r.Error, AlreadyClaimed: r.AlreadyClaimed,
		})
	}
	return out
}

func geminiOutcomes(in []GeminiWebCheckinOutcome) []plugin.AccountOutcome {
	out := make([]plugin.AccountOutcome, 0, len(in))
	for _, r := range in {
		out = append(out, plugin.AccountOutcome{Label: r.Label, Success: r.Success, Message: r.Message, Error: r.Error})
	}
	return out
}

// ---------------------------------------------------------------------------
// raccoon

type raccoonOps struct{ g *Gateway }

func (o raccoonOps) Supports(a plugin.ActionName) bool { return declaresAction("raccoon", a) }

func (o raccoonOps) ListAccounts(ctx context.Context, providerName string, withBalance bool) ([]map[string]any, error) {
	if !withBalance {
		return o.g.genericAccountRows(providerName), nil
	}
	return o.g.RaccoonAccounts(ctx, providerName)
}

func (o raccoonOps) RemoveAccount(providerName, label string) bool {
	return o.g.RaccoonRemoveAccount(providerName, label)
}

func (o raccoonOps) HandleAction(ctx context.Context, providerName string, req plugin.ActionRequest) (plugin.ActionResult, error) {
	switch req.Action {
	case plugin.ActionCheckin:
		res, err := o.g.RaccoonCheckin(ctx, providerName, actionLabel(req))
		if err != nil {
			return plugin.ActionResult{}, err
		}
		return plugin.ActionResult{Data: raccoonOutcomes(res)}, nil
	default:
		return plugin.ActionResult{}, fmt.Errorf("raccoon 不支持操作 %q", req.Action)
	}
}

// ---------------------------------------------------------------------------
// runable

type runableOps struct{ g *Gateway }

func (o runableOps) Supports(a plugin.ActionName) bool { return declaresAction("runable", a) }

func (o runableOps) ListAccounts(ctx context.Context, providerName string, withBalance bool) ([]map[string]any, error) {
	if !withBalance {
		return o.g.genericAccountRows(providerName), nil
	}
	return o.g.RunableAccounts(ctx, providerName)
}

func (o runableOps) RemoveAccount(providerName, label string) bool {
	return o.g.RaccoonRemoveAccount(providerName, label)
}

func (o runableOps) HandleAction(ctx context.Context, providerName string, req plugin.ActionRequest) (plugin.ActionResult, error) {
	switch req.Action {
	case plugin.ActionCheckin:
		res, err := o.g.RunableCheckin(ctx, providerName, actionLabel(req))
		if err != nil {
			return plugin.ActionResult{}, err
		}
		return plugin.ActionResult{Data: runableOutcomes(res)}, nil
	case plugin.ActionVerify, plugin.ActionLogin:
		// Login and Verify are the same server-side probe here: check the pasted
		// cookie (or email/password) against the upstream and return the identity.
		// Login is driven by the panel's neutral /omni/account/login endpoint, which
		// then persists the account itself.
		data, err := o.g.RunableVerify(ctx, providerName, req.Params["email"], req.Params["password"], req.Params["cookie"])
		if err != nil {
			return plugin.ActionResult{}, err
		}
		return plugin.ActionResult{Data: data}, nil
	default:
		return plugin.ActionResult{}, fmt.Errorf("runable 不支持操作 %q", req.Action)
	}
}

// ---------------------------------------------------------------------------
// trae

type traeOps struct{ g *Gateway }

func (o traeOps) Supports(a plugin.ActionName) bool { return declaresAction("trae", a) }

func (o traeOps) ListAccounts(ctx context.Context, providerName string, withBalance bool) ([]map[string]any, error) {
	if !withBalance {
		return o.g.genericAccountRows(providerName), nil
	}
	return o.g.TraeAccounts(ctx, providerName)
}

func (o traeOps) RemoveAccount(providerName, label string) bool {
	return o.g.TraeRemoveAccount(providerName, label)
}

func (o traeOps) HandleAction(ctx context.Context, providerName string, req plugin.ActionRequest) (plugin.ActionResult, error) {
	switch req.Action {
	case plugin.ActionCheckin:
		res, err := o.g.TraeCheckin(ctx, providerName, actionLabel(req))
		if err != nil {
			return plugin.ActionResult{}, err
		}
		return plugin.ActionResult{Data: traeOutcomes(res)}, nil
	default:
		return plugin.ActionResult{}, fmt.Errorf("trae 不支持操作 %q", req.Action)
	}
}

// ---------------------------------------------------------------------------
// newapi

type newapiOps struct{ g *Gateway }

func (o newapiOps) Supports(a plugin.ActionName) bool { return declaresAction("newapi", a) }

func (o newapiOps) ListAccounts(ctx context.Context, providerName string, withBalance bool) ([]map[string]any, error) {
	// newapi always returns the account list; withBalance additionally queries the
	// upstream quota.
	return o.g.NewAPIAccounts(ctx, providerName, withBalance)
}

func (o newapiOps) RemoveAccount(providerName, label string) bool {
	return o.g.NewAPIRemoveAccount(providerName, label)
}

func (o newapiOps) HandleAction(ctx context.Context, providerName string, req plugin.ActionRequest) (plugin.ActionResult, error) {
	switch req.Action {
	case plugin.ActionCheckin:
		res, err := o.g.NewAPICheckin(ctx, providerName, actionLabel(req))
		if err != nil {
			return plugin.ActionResult{}, err
		}
		return plugin.ActionResult{Data: raccoonOutcomes(res)}, nil
	case plugin.ActionRefresh:
		label := actionLabel(req)
		if label != "" {
			if err := o.g.NewAPIRefreshAccount(ctx, providerName, label); err != nil {
				return plugin.ActionResult{}, err
			}
			return plugin.ActionResult{Data: map[string]any{"ok": true, "refreshed": 1}}, nil
		}
		res, err := o.g.NewAPIRefreshAll(ctx, providerName)
		if err != nil {
			return plugin.ActionResult{}, err
		}
		return plugin.ActionResult{Data: raccoonOutcomes(res)}, nil
	default:
		return plugin.ActionResult{}, fmt.Errorf("newapi 不支持操作 %q", req.Action)
	}
}

// ---------------------------------------------------------------------------
// geminiweb

type geminiwebOps struct{ g *Gateway }

func (o geminiwebOps) Supports(a plugin.ActionName) bool { return declaresAction("geminiweb", a) }

func (o geminiwebOps) ListAccounts(ctx context.Context, providerName string, withBalance bool) ([]map[string]any, error) {
	if !withBalance {
		return o.g.genericAccountRows(providerName), nil
	}
	return o.g.GeminiWebAccounts(ctx, providerName)
}

func (o geminiwebOps) RemoveAccount(providerName, label string) bool {
	return o.g.GeminiWebRemoveAccount(providerName, label)
}

func (o geminiwebOps) HandleAction(ctx context.Context, providerName string, req plugin.ActionRequest) (plugin.ActionResult, error) {
	switch req.Action {
	case plugin.ActionCheckin, plugin.ActionRefresh:
		// Gemini Web has no check-in reward and no token rotation: both actions are
		// a session probe that persists any cookie the upstream rotated.
		res, err := o.g.GeminiWebCheckin(ctx, providerName, actionLabel(req))
		if err != nil {
			return plugin.ActionResult{}, err
		}
		return plugin.ActionResult{Data: geminiOutcomes(res)}, nil
	default:
		return plugin.ActionResult{}, fmt.Errorf("geminiweb 不支持操作 %q", req.Action)
	}
}

// ---------------------------------------------------------------------------
// opencode

// opencodeOps exposes OpenCode's provider-scoped quota query through the same
// action path as every other type. OpenCode is a key-pool provider
// (AccountModeNone): it has no accounts to list or maintain — only the Go 套餐
// keys whose remaining quota the panel reads on demand. Quota is provider-scoped,
// so req.Account is nil and no label is read.
type opencodeOps struct{ g *Gateway }

func (o opencodeOps) Supports(a plugin.ActionName) bool { return declaresAction("opencode", a) }

func (o opencodeOps) ListAccounts(_ context.Context, providerName string, _ bool) ([]map[string]any, error) {
	return o.g.genericAccountRows(providerName), nil
}

func (o opencodeOps) RemoveAccount(providerName, label string) bool {
	return o.g.RaccoonRemoveAccount(providerName, label)
}

func (o opencodeOps) HandleAction(ctx context.Context, providerName string, req plugin.ActionRequest) (plugin.ActionResult, error) {
	switch req.Action {
	case plugin.ActionQuota:
		res, err := o.g.OpencodeQuota(ctx, providerName)
		if err != nil {
			return plugin.ActionResult{}, err
		}
		return plugin.ActionResult{Data: res}, nil
	default:
		return plugin.ActionResult{}, fmt.Errorf("opencode 不支持操作 %q", req.Action)
	}
}

// ---------------------------------------------------------------------------
// generic (no declared maintenance)

type genericOps struct{ g *Gateway }

func (o genericOps) Supports(plugin.ActionName) bool { return false }

func (o genericOps) ListAccounts(_ context.Context, providerName string, _ bool) ([]map[string]any, error) {
	return o.g.genericAccountRows(providerName), nil
}

func (o genericOps) RemoveAccount(providerName, label string) bool {
	return o.g.RaccoonRemoveAccount(providerName, label)
}

func (o genericOps) HandleAction(_ context.Context, _ string, req plugin.ActionRequest) (plugin.ActionResult, error) {
	return plugin.ActionResult{}, fmt.Errorf("该供应商不支持操作 %q", req.Action)
}
