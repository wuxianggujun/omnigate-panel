package gateway

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// nonChatHints mark a pool model as NOT suitable for a chat/tool fallback.
// Matched case-insensitively against the model name; kept conservative — only
// clearly non-text modalities — so real chat models are never excluded. Ported
// from gemini2api app/core/fallback.py (_NON_CHAT_HINTS).
var nonChatHints = []string{
	"image", "video", "audio", "speech", "voice", "tts", "stt",
	"whisper", "embed", "rerank", "dall-e", "dalle", "diffusion",
	"imagen", "veo", "sora", "midjourney", "kling", "moderation", "ocr",
}

// looksChatCapable mirrors the reference heuristic: true unless the model name
// clearly denotes a non-chat modality.
func looksChatCapable(model string) bool {
	m := strings.ToLower(model)
	for _, h := range nonChatHints {
		if strings.Contains(m, h) {
			return false
		}
	}
	return true
}

// fallbackCandidate is one third-party model to try as a fallback.
type fallbackCandidate struct {
	Provider string // provider name (routing prefix)
	Model    string // bare upstream model id
}

// qualified is the "provider/model" name used for logging.
func (c fallbackCandidate) qualified() string { return c.Provider + "/" + c.Model }

// fallbackFailure 汇总一次兜底尝试里所有候选的失败。调用方原先只把主目标的
// 错误写回客户端，于是「主目标 429 → 已切到 yijia → yijia 也额度耗尽」在响应
// 里看起来像「根本没有切换」。备用目标全部失败时必须把这次尝试也说出来。
type fallbackFailure struct {
	Attempts []fallbackAttempt
}

type fallbackAttempt struct {
	Candidate fallbackCandidate
	Err       error
}

// Error 只给出候选与失败类别，完整上游正文已经由每个候选自己的 Warn 日志记录；
// 响应里再展开一遍长 JSON 只会把真正有用的主目标错误淹没。
func (e *fallbackFailure) Error() string {
	if e == nil || len(e.Attempts) == 0 {
		return "备用目标均失败"
	}
	parts := make([]string, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		label := a.Candidate.qualified()
		if a.Err != nil {
			if reason := routeFailureReason(a.Err); reason != "" {
				label += ": " + reason
			}
		}
		parts = append(parts, label)
	}
	return "备用目标均失败（" + strings.Join(parts, "；") + "）"
}

// joinFallbackFailure 保留主目标错误（含上游状态码，便于 errors.As 继续识别），
// 在正文末尾补一句兜底结果。没有主目标错误时直接返回兜底汇总。
func joinFallbackFailure(primary error, fallback error) error {
	if fallback == nil {
		return primary
	}
	if primary == nil {
		return fallback
	}
	return fmt.Errorf("%w；%s", primary, fallback.Error())
}

// fallbackCandidates resolves the ordered candidate list for a failed primary
// request (app/core/fallback.py get_fallback_entries). Pinned
// cfg.fallback.models — when set — are tried in the given order; otherwise every
// chat-capable model on a non-Gemini provider is used in random order. The
// primary provider/model is always excluded (defence against self-routing).
func (g *Gateway) fallbackCandidates(ctx context.Context, primaryProvider, primaryModel string) []fallbackCandidate {
	if !g.cfg.Fallback.On() {
		return nil
	}
	exclude := map[string]bool{primaryProvider + "/" + primaryModel: true}

	// collect gathers every non-Gemini, provider-qualified model.
	collect := func() ([]fallbackCandidate, map[string]fallbackCandidate) {
		var all []fallbackCandidate
		byName := map[string]fallbackCandidate{}
		for _, name := range g.order {
			pcfg := g.cfg.Provider(name)
			if pcfg == nil || traitsOf(pcfg.Type).FallbackExcluded {
				continue
			}
			models, err := g.models(ctx, name)
			if err != nil {
				continue
			}
			for _, m := range models {
				q := name + "/" + m.ID
				if exclude[q] {
					continue
				}
				c := fallbackCandidate{Provider: name, Model: m.ID}
				all = append(all, c)
				if _, ok := byName[q]; !ok {
					byName[q] = c
				}
				// Also index the bare id so an operator can pin "gpt-4o".
				if _, ok := byName[m.ID]; !ok {
					byName[m.ID] = c
				}
			}
		}
		return all, byName
	}

	all, byName := collect()
	if len(g.cfg.Fallback.Models) > 0 {
		out := make([]fallbackCandidate, 0, len(g.cfg.Fallback.Models))
		for _, n := range g.cfg.Fallback.Models {
			if c, ok := byName[strings.TrimSpace(n)]; ok {
				out = append(out, c)
			}
		}
		return out
	}
	out := make([]fallbackCandidate, 0, len(all))
	for _, c := range all {
		if looksChatCapable(c.Model) {
			out = append(out, c)
		}
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// resultHasOutput reports whether a chat result carries anything usable: text,
// tool calls, or reasoning. Mirrors openai_data_is_empty (inverted).
func resultHasOutput(res *chatResult) bool {
	if res == nil {
		return false
	}
	return strings.TrimSpace(res.text) != "" || len(res.toolCalls) > 0 || strings.TrimSpace(res.reasoning) != ""
}

// tryFallback runs the fallback chain for a failed/empty primary request and
// returns the winning result, or nil when every candidate failed or produced
// nothing ("one fails → try another"). The second return value is a compact
// summary of the candidates that failed, so the caller can tell the client that
// failover was attempted instead of echoing only the primary error.
//
// chain holds the explicit candidates declared by a model alias chain (见
// modelmap.go / modelChainFallbacks): they are tried FIRST and in order, so an
// operator can drain an expiring account (prefer raccoon → fall back to
// runable) without turning on the generic third-party fallback. The generic
// cfg.Fallback candidates are appended afterwards and only when enabled.
func (g *Gateway) tryFallback(ctx context.Context, req *openai.ChatRequest, primaryProvider, primaryModel string, hasTools bool, chain []fallbackCandidate) (*chatResult, error) {
	// 入站密钥的收窄范围：主目标已在入口校验过（allowModel），但引流链的后续
	// 目标和全局 cfg.Fallback 的候选没走过那道校验 —— 不在这里收口，一个只授了
	// raccoon 的 key 就能顺着链打到 runable，「按供应商收窄」等于没做。
	meta := ReqMetaFrom(ctx)
	candidates := make([]fallbackCandidate, 0, len(chain)+4)
	seen := make(map[string]bool, len(chain)+4)
	add := func(c fallbackCandidate, src string) {
		if c.Provider == "" || c.Model == "" || seen[c.qualified()] {
			return
		}
		if !meta.ProviderAllowed(c.Provider) {
			g.log.Info("跳过%s候选 %s：api key 未授权该供应商", src, c.qualified())
			return
		}
		seen[c.qualified()] = true
		candidates = append(candidates, c)
	}
	for _, c := range chain {
		add(c, "引流链")
	}
	if g.cfg.Fallback.On() {
		for _, c := range g.fallbackCandidates(ctx, primaryProvider, primaryModel) {
			add(c, "全局兜底")
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	incognito := g.cfg.Mode == "incognito"
	attempts := make([]fallbackAttempt, 0, len(candidates))
	for _, c := range candidates {
		if ctx.Err() != nil {
			attempts = append(attempts, fallbackAttempt{Candidate: c, Err: ctx.Err()})
			break
		}
		prov := g.providers[c.Provider]
		if prov == nil {
			attempts = append(attempts, fallbackAttempt{
				Candidate: c,
				Err:       fmt.Errorf("provider 未注册"),
			})
			continue
		}
		in := g.buildInput(prov, c.Model, req, incognito, hasTools)
		res, err := g.runChat(ctx, c.Provider, prov, in, hasTools, nil, nil)
		if err != nil {
			if ctx.Err() == nil {
				g.markRouteFailure(c.Provider, c.Model, err, false, time.Now())
			}
			g.log.Warn("兜底 %s 失败: %v", c.qualified(), err)
			attempts = append(attempts, fallbackAttempt{Candidate: c, Err: err})
			continue
		}
		if !resultHasOutput(res) {
			// 上游正常收尾但正文为空（例如 max_tokens 太小）不是供应商故障：
			// 仍然试下一个候选拿正文，但不要把这家记进绕行窗口。
			if ctx.Err() == nil && (res == nil || !res.sawFinish) {
				g.markRouteFailure(c.Provider, c.Model, errEmptyUpstream, false, time.Now())
			} else {
				g.clearRouteFailure(c.Provider, c.Model)
			}
			g.log.Warn("兜底 %s 返回空，试下一个", c.qualified())
			attempts = append(attempts, fallbackAttempt{Candidate: c, Err: errEmptyUpstream})
			continue
		}
		g.clearRouteFailure(c.Provider, c.Model)
		// Attribute the request to the provider that actually served it.
		// meta 可能为 nil：HandleChat 是导出的网关入口，单元测试/嵌入式调用
		// 不一定经过 serveLogged。范围判断已由 ProviderAllowed 的 nil 接收器
		// 兜住，这里也不能因为记账对象缺失而 panic。
		if meta != nil {
			meta.Provider = c.Provider
			// 兜底命中后上游已换成候选的模型，真实模型名必须跟着走，否则请求记录
			// 会显示一个根本没被调用的模型（主目标那个）。
			meta.UpstreamModel = c.Model
		}
		g.log.Info("兜底命中 %s", c.qualified())
		return res, nil
	}
	failure := &fallbackFailure{Attempts: attempts}
	if len(attempts) > 0 {
		g.log.Warn("%s", failure.Error())
	}
	return nil, failure
}
