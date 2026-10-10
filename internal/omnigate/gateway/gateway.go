// Package gateway routes OpenAI-compatible requests to configured providers,
// manages the account pool, and runs check-in / keep-alive tasks.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/openaibackend"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/raccoon"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/runable"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/toolcall"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
)

// Gateway is the routing core.
type Gateway struct {
	cfg *config.Config
	st  *state.State
	log *logx.Logger

	providers map[string]provider.Provider
	order     []string
	accounts  map[string][]*provider.Account

	accountsMu sync.RWMutex

	modelMu    sync.Mutex
	modelCache map[string]modelEntry

	convMu sync.Mutex
	convs  map[string]*conv

	pendingMu sync.Mutex
	pending   map[string]raccoonPending

	// proxyFor resolves a per-provider outbound proxy (nil = direct). Installed
	// via WithProxyResolver; consulted once per provider at construction.
	proxyFor func(providerName string) *url.URL
}

// raccoonPending remembers an in-flight browser authorization.
type raccoonPending struct {
	provider string
	label    string
	at       time.Time
}

type modelEntry struct {
	models []openai.Model
	at     time.Time
}

type conv struct {
	chatID     string
	systemSent bool
	at         time.Time
}

type chatResult struct {
	text      string
	reasoning string
	finish    string
	toolCalls []openai.ToolCall
	errText   string
}

// Option configures the Gateway at construction time.
type Option func(*Gateway)

// WithProxyResolver installs a per-provider outbound proxy resolver. It is
// consulted once per provider while building the provider map; returning nil
// means direct connection. Providers that support proxying implement SetProxy.
func WithProxyResolver(fn func(providerName string) *url.URL) Option {
	return func(g *Gateway) { g.proxyFor = fn }
}

// New builds a Gateway from config and state.
func New(cfg *config.Config, st *state.State, log *logx.Logger, opts ...Option) (*Gateway, error) {
	g := &Gateway{
		cfg:        cfg,
		st:         st,
		log:        log,
		providers:  map[string]provider.Provider{},
		accounts:   map[string][]*provider.Account{},
		modelCache: map[string]modelEntry{},
		convs:      map[string]*conv{},
		pending:    map[string]raccoonPending{},
	}
	for _, o := range opts {
		o(g)
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		var prov provider.Provider
		switch p.Type {
		case "runable":
			prov = runable.NewProvider(p.Name, p.BaseURL, p.DeviceHeader)
		case "openai":
			prov = openaibackend.New(p.Name, p.BaseURL, p.APIKey, p.Models)
		case "raccoon":
			prov = raccoon.NewProvider(p.Name, p.MainOrigin, p.LLMBase, p.AuthBase)
		default:
			return nil, fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type)
		}
		// Per-provider outbound proxy (panel config outbound.routes).
		if g.proxyFor != nil {
			if u := g.proxyFor(p.Name); u != nil {
				if ps, ok := prov.(interface{ SetProxy(*url.URL) }); ok {
					ps.SetProxy(u)
				}
			}
		}
		g.providers[p.Name] = prov
		g.order = append(g.order, p.Name)

		var accts []*provider.Account
		seen := map[string]bool{}
		for _, a := range p.Accounts {
			label := a.Label
			if label == "" {
				label = fmt.Sprintf("账号 %d", len(accts)+1)
			}
			seen[label] = true
			accts = append(accts, &provider.Account{
				Label:        label,
				Cookie:       st.Cookie(p.Name, label, a.Cookie),
				Email:        a.Email,
				Password:     a.Password,
				RefreshToken: st.RefreshToken(p.Name, label, a.RefreshToken),
			})
		}
		// Merge accounts added at runtime (e.g. via the Raccoon web login).
		for _, d := range st.DynAccounts(p.Name) {
			if d.Label == "" || seen[d.Label] {
				continue
			}
			seen[d.Label] = true
			accts = append(accts, &provider.Account{
				Label:        d.Label,
				Cookie:       st.Cookie(p.Name, d.Label, d.AccessToken),
				RefreshToken: st.RefreshToken(p.Name, d.Label, d.RefreshToken),
			})
		}
		g.accounts[p.Name] = accts
	}
	return g, nil
}

// Config exposes the loaded config.
func (g *Gateway) Config() *config.Config { return g.cfg }

// Logger exposes the logger.
func (g *Gateway) Logger() *logx.Logger { return g.log }

// ProviderNames returns configured provider names in order.
func (g *Gateway) ProviderNames() []string { return g.order }

// ProviderType returns the type of a provider.
func (g *Gateway) ProviderType(name string) string {
	if p := g.providers[name]; p != nil {
		return p.Type()
	}
	return ""
}

// Accounts returns the account pool for a provider.
func (g *Gateway) Accounts(name string) []*provider.Account {
	g.accountsMu.RLock()
	defer g.accountsMu.RUnlock()
	return g.accounts[name]
}

// models returns a provider's models, cached for 10 minutes.
func (g *Gateway) models(ctx context.Context, name string) ([]openai.Model, error) {
	g.modelMu.Lock()
	if e, ok := g.modelCache[name]; ok && time.Since(e.at) < 10*time.Minute {
		g.modelMu.Unlock()
		return e.models, nil
	}
	g.modelMu.Unlock()

	prov := g.providers[name]
	if prov == nil {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	models, err := prov.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	g.modelMu.Lock()
	g.modelCache[name] = modelEntry{models: models, at: time.Now()}
	g.modelMu.Unlock()
	return models, nil
}

// ListModels aggregates every provider's models, prefixed with "provider/".
func (g *Gateway) ListModels(ctx context.Context) []openai.Model {
	out := []openai.Model{}
	for _, name := range g.order {
		models, err := g.models(ctx, name)
		if err != nil {
			g.log.Warn("获取 %s 模型失败: %v", name, err)
			continue
		}
		for _, m := range models {
			m.ID = name + "/" + m.ID
			if m.OwnedBy == "" {
				m.OwnedBy = name
			}
			out = append(out, m)
		}
	}
	return out
}

// route splits a requested model into (provider, upstream model).
func (g *Gateway) route(model string) (string, string) {
	if model == "" {
		return g.cfg.DefaultProvider, g.cfg.DefaultModel
	}
	if i := strings.Index(model, "/"); i > 0 {
		if _, ok := g.providers[model[:i]]; ok {
			return model[:i], model[i+1:]
		}
	}
	return g.cfg.DefaultProvider, model
}

// resolveModel maps a short name to a full upstream model id.
func (g *Gateway) resolveModel(ctx context.Context, providerName, model string) string {
	if model == "" {
		return g.cfg.DefaultModel
	}
	models, err := g.models(ctx, providerName)
	if err == nil {
		for _, m := range models {
			if m.ID == model {
				return model
			}
		}
		for _, m := range models {
			if i := strings.LastIndex(m.ID, "/"); i >= 0 && m.ID[i+1:] == model {
				return m.ID
			}
		}
	}
	if g.ProviderType(providerName) == "runable" && g.cfg.DefaultModel != "" {
		return g.cfg.DefaultModel
	}
	return model
}

// HandleChat serves POST /v1/chat/completions.
func (g *Gateway) HandleChat(ctx context.Context, req *openai.ChatRequest, w http.ResponseWriter) {
	if len(req.Messages) == 0 {
		writeJSON(w, 400, openai.ErrorJSON("messages_required", "messages is required"))
		return
	}
	provName, model := g.route(req.Model)
	prov := g.providers[provName]
	if prov == nil {
		writeJSON(w, 400, openai.ErrorJSON("unknown_provider", "未知 provider: "+provName))
		return
	}
	upstreamModel := g.resolveModel(ctx, provName, model)
	incognito := g.cfg.Mode == "incognito"
	hasTools := len(req.Tools) > 0

	displayModel := req.Model
	if displayModel == "" {
		displayModel = provName + "/" + upstreamModel
	}

	id := openai.NewID()
	created := time.Now().Unix()

	if req.Stream {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		h.Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		_ = openai.EmitRole(w, id, displayModel, created)
		flush()

		onText := func(s string) {
			_ = openai.EmitContent(w, id, displayModel, created, s)
			flush()
		}
		onReasoning := func(s string) {
			if g.cfg.Reasoning {
				_ = openai.EmitReasoning(w, id, displayModel, created, s)
				flush()
			}
		}
		res, err := g.runChat(ctx, provName, prov, upstreamModel, req, incognito, hasTools, onText, onReasoning)
		if err != nil {
			g.log.Error("对话失败 %s: %v", displayModel, err)
			fmt.Fprintf(w, "data: %s\n\n", openai.ErrorJSON("upstream_error", err.Error()))
			_ = openai.EmitFinish(w, id, displayModel, created, "stop")
			_ = openai.Done(w)
			flush()
			return
		}
		if len(res.toolCalls) > 0 {
			_ = openai.EmitToolCalls(w, id, displayModel, created, res.toolCalls)
		}
		_ = openai.EmitFinish(w, id, displayModel, created, res.finish)
		_ = openai.Done(w)
		flush()
		g.log.Info("✓ 完成 %s · 流式 · %s", displayModel, res.finish)
		return
	}

	res, err := g.runChat(ctx, provName, prov, upstreamModel, req, incognito, hasTools, nil, nil)
	if err != nil {
		g.log.Error("对话失败 %s: %v", displayModel, err)
		writeJSON(w, 502, openai.ErrorJSON("upstream_error", err.Error()))
		return
	}
	msg := openai.Message{Role: "assistant"}
	if res.text != "" {
		msg.Content = openai.Content{Text: res.text}
	}
	if len(res.toolCalls) > 0 {
		msg.ToolCalls = res.toolCalls
	}
	writeJSON(w, 200, openai.Completion(id, displayModel, created, msg, res.finish))
	g.log.Info("✓ 完成 %s · %s", displayModel, res.finish)
}

func (g *Gateway) runChat(ctx context.Context, provName string, prov provider.Provider, model string, req *openai.ChatRequest, incognito, hasTools bool, onText, onReasoning func(string)) (*chatResult, error) {
	res := &chatResult{finish: "stop"}

	// Prompt-based providers need emulated tool calling.
	if hasTools && prov.Type() == "runable" {
		prompt := toolcall.BuildToolPrompt(req.Messages, req.Tools)
		in := provider.ChatInput{ChatID: util.UUID(), Model: model, Prompt: prompt, Incognito: incognito, Mode: g.cfg.Mode}
		stream, err := g.openStream(ctx, provName, prov, in)
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		parser := toolcall.NewParser(
			func(s string) {
				res.text += s
				if onText != nil {
					onText(s)
				}
			},
			func(c openai.ToolCall) { res.toolCalls = append(res.toolCalls, c) },
		)
		err = g.drain(ctx, stream, func(e provider.Event) {
			switch e.Type {
			case provider.EventText:
				parser.Feed(e.Text)
			case provider.EventReasoning:
				res.reasoning += e.Text
				if onReasoning != nil {
					onReasoning(e.Text)
				}
			case provider.EventFinish:
				res.finish = e.Finish
			case provider.EventError:
				res.errText = e.Text
			}
		})
		parser.Flush()
		if err != nil {
			return nil, err
		}
		if res.errText != "" {
			return nil, fmt.Errorf("%s", res.errText)
		}
		if len(res.toolCalls) > 0 {
			res.finish = "tool_calls"
		}
		return res, nil
	}

	var in provider.ChatInput
	in.Model = model
	in.Incognito = incognito
	in.Mode = g.cfg.Mode
	if hasTools {
		in.Messages = req.Messages
		in.Tools = req.Tools
	} else if prov.Type() == "runable" {
		c := g.getConv(toolcall.ConvKey(model, req.Messages))
		prompt := toolcall.LastUserText(req.Messages)
		if !c.systemSent {
			if sys := toolcall.SystemText(req.Messages); sys != "" {
				prompt = sys + "\n\n" + prompt
			}
			c.systemSent = true
		}
		if strings.TrimSpace(prompt) == "" {
			prompt = "你好"
		}
		in.ChatID = c.chatID
		in.Prompt = prompt
	} else {
		in.Messages = req.Messages
	}

	stream, err := g.openStream(ctx, provName, prov, in)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	err = g.drain(ctx, stream, func(e provider.Event) {
		switch e.Type {
		case provider.EventText:
			res.text += e.Text
			if onText != nil {
				onText(e.Text)
			}
		case provider.EventReasoning:
			res.reasoning += e.Text
			if onReasoning != nil {
				onReasoning(e.Text)
			}
		case provider.EventToolCalls:
			res.toolCalls = append(res.toolCalls, e.ToolCalls...)
		case provider.EventFinish:
			res.finish = e.Finish
		case provider.EventError:
			res.errText = e.Text
		}
	})
	if err != nil {
		return nil, err
	}
	if res.errText != "" {
		return nil, fmt.Errorf("%s", res.errText)
	}
	if len(res.toolCalls) > 0 {
		res.finish = "tool_calls"
	}
	return res, nil
}

// openStream picks an account (switching on exhausted credits) and opens the stream.
func (g *Gateway) openStream(ctx context.Context, provName string, prov provider.Provider, in provider.ChatInput) (provider.Stream, error) {
	g.accountsMu.RLock()
	accts := g.accounts[provName]
	g.accountsMu.RUnlock()

	// Stateless upstreams (api-key based) need no account pool.
	if len(accts) == 0 {
		if prov.Type() == "openai" {
			return prov.StreamChat(ctx, &provider.Account{}, in)
		}
		return nil, fmt.Errorf("provider %q has no accounts configured", provName)
	}

	n := len(accts)
	start := g.st.ActiveIndex(provName)
	if start < 0 || start >= n {
		start = 0
	}
	var lastErr error
	for k := 0; k < n; k++ {
		idx := (start + k) % n
		acc := accts[idx]
		// Token-based providers refresh before the call when the access token
		// is missing or about to expire.
		if rp, ok := prov.(*raccoon.Provider); ok && acc.RefreshToken != "" && rp.TokenExpired(acc) {
			if err := g.refreshRaccoonToken(ctx, provName, prov, acc); err != nil {
				g.log.Warn("账号「%s」刷新 token 失败: %v", acc.Label, err)
			}
		}
		if acc.Cookie == "" {
			if !g.tryLogin(ctx, provName, prov, acc) {
				continue
			}
		}
		stream, err := prov.StreamChat(ctx, acc, in)
		// On an auth failure, refresh once and retry.
		if err != nil && prov.Type() == "raccoon" && raccoon.Unauthorized(err) {
			if rerr := g.refreshRaccoonToken(ctx, provName, prov, acc); rerr == nil {
				stream, err = prov.StreamChat(ctx, acc, in)
			}
		}
		if err == nil {
			if idx != start {
				g.st.SetActive(provName, idx)
				g.log.Info("⇄ 自动切换账号「%s」", acc.Label)
			}
			return stream, nil
		}
		lastErr = err
		if !provider.OutOfCredits(err) {
			return nil, err
		}
		g.log.Warn("账号「%s」积分耗尽，尝试下一个", acc.Label)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("provider %q has no usable account", provName)
	}
	return nil, lastErr
}

// tryLogin refreshes an account cookie from stored credentials.
func (g *Gateway) tryLogin(ctx context.Context, provName string, prov provider.Provider, acc *provider.Account) bool {
	if acc.Email == "" || acc.Password == "" {
		return false
	}
	rp, ok := prov.(*runable.Provider)
	if !ok {
		return false
	}
	cookie, err := rp.Client().SignIn(ctx, acc.Email, acc.Password)
	if err != nil {
		g.log.Warn("账号「%s」登录失败: %v", acc.Label, err)
		return false
	}
	acc.Cookie = cookie
	g.st.SetCookie(provName, acc.Label, cookie)
	return true
}

func (g *Gateway) drain(ctx context.Context, s provider.Stream, fn func(provider.Event)) error {
	for {
		e, err := s.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		fn(e)
		if e.Type == provider.EventFinish {
			return nil
		}
	}
}

func (g *Gateway) getConv(key string) *conv {
	g.convMu.Lock()
	defer g.convMu.Unlock()
	if len(g.convs) > 4096 {
		cutoff := time.Now().Add(-2 * time.Hour)
		for k, c := range g.convs {
			if c.at.Before(cutoff) {
				delete(g.convs, k)
			}
		}
	}
	c := g.convs[key]
	if c == nil {
		c = &conv{chatID: util.UUID()}
		g.convs[key] = c
	}
	c.at = time.Now()
	return c
}

// RunRunableCheckin validates every Runable account, refreshes cookies when
// possible, and records the current credit balance.
func (g *Gateway) RunRunableCheckin(providerName string) {
	pcfg := g.cfg.Provider(providerName)
	if pcfg == nil || pcfg.Type != "runable" {
		return
	}
	rp, ok := g.providers[providerName].(*runable.Provider)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, acc := range g.accounts[providerName] {
		cookie := acc.Cookie
		if cookie == "" && acc.Email != "" && acc.Password != "" {
			c, err := rp.Client().SignIn(ctx, acc.Email, acc.Password)
			if err != nil {
				g.log.Warn("签到：账号「%s」登录失败: %v", acc.Label, err)
				continue
			}
			cookie = c
		}
		if cookie == "" {
			g.log.Warn("签到：账号「%s」没有可用凭证", acc.Label)
			continue
		}
		sess := rp.Client().FetchSession(ctx, cookie)
		if !sess.Alive && acc.Email != "" && acc.Password != "" {
			c, err := rp.Client().SignIn(ctx, acc.Email, acc.Password)
			if err == nil {
				cookie = c
				sess = rp.Client().FetchSession(ctx, cookie)
			}
		}
		if !sess.Alive {
			g.log.Warn("签到：账号「%s」会话无效或已过期", acc.Label)
			continue
		}
		acc.Cookie = cookie
		g.st.SetCookie(providerName, acc.Label, cookie)
		cr := rp.Client().FetchCredits(ctx, cookie)
		if cr.OK {
			g.log.Info("签到/保活 %s「%s」会话有效，积分 %d（日 %d + 月 %d）", providerName, acc.Label, cr.Total, cr.Daily, cr.Monthly)
		} else {
			g.log.Warn("签到：账号「%s」会话有效，但读取积分失败: %s", acc.Label, cr.Error)
		}
	}
}

// RunableAccounts lists a Runable provider's accounts with a best-effort credit
// balance (available = 每日额度 + 每月额度). The panel account pool asks for this
// on demand (withBalance); the plain listing stays a cheap in-memory view so the
// 5s overview poll never hits the upstream. Accounts without a usable cookie are
// returned without a balance rather than failing the whole listing.
func (g *Gateway) RunableAccounts(ctx context.Context, providerName string) ([]map[string]any, error) {
	pcfg := g.cfg.Provider(providerName)
	if pcfg == nil || pcfg.Type != "runable" {
		return nil, fmt.Errorf("provider %q 不是 runable 类型", providerName)
	}
	rp, ok := g.providers[providerName].(*runable.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q 不是 runable 类型", providerName)
	}
	out := []map[string]any{}
	for _, acc := range g.Accounts(providerName) {
		item := map[string]any{"label": acc.Label, "has_token": acc.Cookie != ""}
		cookie := acc.Cookie
		if cookie == "" && acc.Email != "" && acc.Password != "" {
			if c, err := rp.Client().SignIn(ctx, acc.Email, acc.Password); err == nil {
				cookie = c
				acc.Cookie = c
				g.st.SetCookie(providerName, acc.Label, c)
			}
		}
		if cookie != "" {
			bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			cr := rp.Client().FetchCredits(bctx, cookie)
			cancel()
			if cr.OK {
				item["available"] = cr.Total
				item["wallets"] = []map[string]any{
					{"type": "monthly_credits", "displayName": "每月额度", "balance": cr.Monthly},
					{"type": "daily_credits", "displayName": "每日额度", "balance": cr.Daily},
				}
			} else if cr.Error != "" {
				item["balance_error"] = cr.Error
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// RunableVerify 校验 runable 账号并返回账号身份 + 当前积分（用于面板「浏览器登录」）。
//
// runable 只支持 Google/Facebook 登录（邮箱密码已全局关闭：服务端直连会得到
// EMAIL_PASSWORD_DISABLED），且登录产物是 runable.com 域下的 httpOnly Cookie——任何
// 网页（含面板）都读不到，Google 也不会把结果交回面板。所以面板无法自动登录/抓取，
// 只能由用户在浏览器登录 runable.com 后，把 session_token 贴进来，这里做一次服务端
// 校验（get-session）并取积分。
//
// 若给了 email/password（runable 未来若重开密码登录）则先登录再校验；否则直接用 cookie。
func (g *Gateway) RunableVerify(ctx context.Context, providerName, email, password, cookie string) (map[string]any, error) {
	pcfg := g.cfg.Provider(providerName)
	if pcfg == nil || pcfg.Type != "runable" {
		return nil, fmt.Errorf("provider %q 不是 runable 类型", providerName)
	}
	rp, ok := g.providers[providerName].(*runable.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q 不是 runable 类型", providerName)
	}
	cookie = normalizeRunableCookie(cookie)
	email = strings.TrimSpace(email)
	if cookie == "" && (email == "" || password == "") {
		return nil, fmt.Errorf("请粘贴 session_token（Cookie）")
	}
	lctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	if cookie == "" {
		c, err := rp.Client().SignIn(lctx, email, password)
		if err != nil {
			return nil, err
		}
		cookie = c
	}
	sess := rp.Client().FetchSession(lctx, cookie)
	if !sess.Alive {
		return nil, fmt.Errorf("会话无效或已过期：请重新在 runable.com 登录后复制 session_token")
	}
	out := map[string]any{"name": sess.Name, "cookie": cookie}
	if sess.Email != "" {
		out["email"] = sess.Email
	} else {
		out["email"] = email
	}
	if cr := rp.Client().FetchCredits(lctx, cookie); cr.OK {
		out["available"] = cr.Total
		out["monthly"] = cr.Monthly
		out["daily"] = cr.Daily
	}
	return out, nil
}

// normalizeRunableCookie 把用户粘贴的内容规范成可用的 Cookie 头：
//   - 只给 session_token 的值（无 "="）→ 补 "session_token="；
//   - 已含 "session_token=" 或整段 Cookie 头 → 原样返回。
func normalizeRunableCookie(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "=") {
		return raw
	}
	return "session_token=" + raw
}

// LastCheckin returns when a named check-in task last ran.
func (g *Gateway) LastCheckin(name string) time.Time { return g.st.LastCheckinAt(name) }

// MarkCheckin records that a named check-in task just ran.
func (g *Gateway) MarkCheckin(name string) { g.st.MarkCheckin(name) }

// Info returns a small status snapshot for /healthz.
func (g *Gateway) Info(ctx context.Context) map[string]any {
	providers := map[string]any{}
	for _, name := range g.order {
		accts := g.Accounts(name)
		labels := make([]string, 0, len(accts))
		for _, a := range accts {
			state := "no-cookie"
			if a.Cookie != "" {
				state = "ready"
			}
			labels = append(labels, a.Label+"("+state+")")
		}
		providers[name] = map[string]any{
			"type":     g.ProviderType(name),
			"accounts": labels,
			"active":   g.st.ActiveIndex(name),
		}
	}
	return map[string]any{
		"status":    "ok",
		"service":   "omnigate",
		"mode":      g.cfg.Mode,
		"providers": providers,
	}
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// MarshalIndent is a tiny helper used by the CLI to pretty-print JSON.
func MarshalIndent(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}
