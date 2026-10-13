package gateway

import (
	"context"
	"math/rand"
	"strings"

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
			if pcfg == nil || pcfg.Type == "geminiweb" {
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
// nothing ("one fails → try another").
//
// chain holds the explicit candidates declared by a model alias chain (见
// modelmap.go / modelChainFallbacks): they are tried FIRST and in order, so an
// operator can drain an expiring account (prefer raccoon → fall back to
// runable) without turning on the generic third-party fallback. The generic
// cfg.Fallback candidates are appended afterwards and only when enabled.
func (g *Gateway) tryFallback(ctx context.Context, req *openai.ChatRequest, primaryProvider, primaryModel string, hasTools bool, chain []fallbackCandidate) *chatResult {
	candidates := make([]fallbackCandidate, 0, len(chain)+4)
	seen := make(map[string]bool, len(chain)+4)
	for _, c := range chain {
		if c.Provider == "" || c.Model == "" || seen[c.qualified()] {
			continue
		}
		seen[c.qualified()] = true
		candidates = append(candidates, c)
	}
	if g.cfg.Fallback.On() {
		for _, c := range g.fallbackCandidates(ctx, primaryProvider, primaryModel) {
			if seen[c.qualified()] {
				continue
			}
			seen[c.qualified()] = true
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	incognito := g.cfg.Mode == "incognito"
	for _, c := range candidates {
		prov := g.providers[c.Provider]
		if prov == nil {
			continue
		}
		in := g.buildInput(prov, c.Model, req, incognito, hasTools)
		res, err := g.runChat(ctx, c.Provider, prov, in, hasTools, nil, nil)
		if err != nil {
			g.log.Warn("兜底 %s 失败: %v", c.qualified(), err)
			continue
		}
		if !resultHasOutput(res) {
			g.log.Warn("兜底 %s 返回空，试下一个", c.qualified())
			continue
		}
		// Attribute the request to the provider that actually served it.
		if m := ReqMetaFrom(ctx); m != nil {
			m.Provider = c.Provider
		}
		g.log.Info("兜底命中 %s", c.qualified())
		return res
	}
	return nil
}
