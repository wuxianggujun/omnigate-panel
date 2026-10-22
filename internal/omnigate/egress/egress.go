// Package egress defines *who omnigate says it is* when it talks to an upstream.
//
// Every provider used to hardcode its own request headers — openaibackend sent no
// User-Agent at all (so Go's "Go-http-client/1.1" leaked), newapi stamped a
// browser UA, geminiweb had a whole fingerprinting stack, opencode mirrored the
// official CLI. That is four unrelated answers to one question, and it left no
// way for an operator to say "this relay only accepts Claude Code".
//
// This package is the single answer. A Profile is per-provider *configuration*:
// a named preset (browser / claude-code / codex), plus optional overrides for the
// User-Agent, arbitrary headers and the TLS ClientHello. Resolve turns it into
// the concrete header set and TLS profile a transport should apply.
//
// Design notes:
//   - Presets are data, not code paths. A provider never switches on the preset
//     name; it calls Apply and stays generic.
//   - Explicit fields always win over the preset, so an operator can pick
//     "claude-code" and bump the CLI version without a new build.
//   - The zero Profile resolves to PresetDefault: an honest "omnigate/<ver>" UA.
//     Impersonation is opt-in, never a silent default.
package egress

import (
	"net/http"
	"sort"
	"strings"
)

// Version is the version reported by the honest default identity. Bump it when
// the wire shape changes in a way an upstream could notice.
const Version = "1.0"

// Presets understood by Resolve. Empty means PresetDefault.
const (
	// PresetDefault is the honest identity: "omnigate/<ver>". Use it when the
	// upstream has no opinion about its clients (most OpenAI-compatible relays).
	//
	// The empty string also means PresetDefault, and the panel's enum uses
	// PresetHonest explicitly because the schema validator rejects an empty enum
	// value.
	PresetDefault = ""
	// PresetHonest is the panel-visible spelling of PresetDefault.
	PresetHonest = "omnigate"
	// PresetBrowser is a desktop Chrome tab: UA, Accept-Language and the
	// Sec-Fetch-* trio. This is what a relay's own console sends.
	PresetBrowser = "browser"
	// PresetClaudeCode is the Claude Code CLI (claude-cli/…): the identity
	// relays such as AnyRouter/AgentRouter gate on, together with a Chrome TLS
	// ClientHello because the CLI ships a Rust/Node TLS stack, not Go's.
	PresetClaudeCode = "claude-code"
	// PresetCodex is the OpenAI Codex CLI (codex_cli_rs/…). Codex speaks the
	// Responses API; this preset only covers the client identity, so pair it
	// with an upstream that also accepts chat/completions.
	PresetCodex = "codex"
)

// Presets lists the preset names the panel offers (excluding the empty default).
func Presets() []string {
	return []string{PresetBrowser, PresetClaudeCode, PresetCodex}
}

// PresetLabels names the presets for the panel's enum field.
func PresetLabels() []PresetLabel {
	return []PresetLabel{
		{Value: PresetHonest, Label: "omnigate（如实申报）"},
		{Value: PresetBrowser, Label: "桌面 Chrome 浏览器"},
		{Value: PresetClaudeCode, Label: "Claude Code CLI"},
		{Value: PresetCodex, Label: "OpenAI Codex CLI"},
	}
}

// PresetLabel is one enum option for the panel.
type PresetLabel struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ChromeUA is the desktop Chrome User-Agent used by PresetBrowser.
const ChromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// AcceptLanguage accompanies ChromeUA.
const AcceptLanguage = "zh-CN,zh;q=0.9,en;q=0.8"

// ClaudeCodeVersion is the CLI version reported by PresetClaudeCode. Operators
// override it per provider (client.user_agent) when a relay starts rejecting an
// outdated build.
const ClaudeCodeVersion = "2.0.14"

// CodexVersion is the CLI version reported by PresetCodex.
const CodexVersion = "0.48.0"

// Profile is the configurable outbound identity of one provider. It is stored in
// the provider config as "client" and copied verbatim into the plugin spec.
type Profile struct {
	// Preset selects the base identity (see the Preset* constants). Empty =
	// PresetDefault for generic providers; a provider with its own historical
	// identity (newapi → browser) passes its own default to ResolveWith.
	Preset string `json:"preset,omitempty"`
	// UserAgent overrides the preset's UA when non-empty.
	UserAgent string `json:"user_agent,omitempty"`
	// Headers are extra headers set on every request. They win over the preset,
	// so an operator can override e.g. anthropic-beta without a preset change.
	Headers map[string]string `json:"headers,omitempty"`
	// TLSProfile selects the utls ClientHello ("chrome120", …). Empty = the
	// preset's own default ("" for everything except claude-code, which asks for
	// a Chrome hello). "none" explicitly disables TLS impersonation.
	TLSProfile string `json:"tls_profile,omitempty"`
}

// IsZero reports whether the profile carries no configuration at all. Providers
// use it to keep their historical defaults untouched.
func (p Profile) IsZero() bool {
	return p.Preset == "" && p.UserAgent == "" && len(p.Headers) == 0 && p.TLSProfile == ""
}

// Normalized returns the profile with whitespace trimmed and keys canonicalised.
func (p Profile) Normalized() Profile {
	p.Preset = strings.ToLower(strings.TrimSpace(p.Preset))
	p.UserAgent = strings.TrimSpace(p.UserAgent)
	p.TLSProfile = strings.ToLower(strings.TrimSpace(p.TLSProfile))
	if len(p.Headers) > 0 {
		out := make(map[string]string, len(p.Headers))
		for k, v := range p.Headers {
			k = http.CanonicalHeaderKey(strings.TrimSpace(k))
			if k == "" || strings.TrimSpace(v) == "" {
				continue
			}
			out[k] = strings.TrimSpace(v)
		}
		if len(out) == 0 {
			out = nil
		}
		p.Headers = out
	}
	return p
}

// Header is one ordered request header. Order is preserved for logging and for
// transports that write the header block themselves; net/http sorts headers on
// the wire, which no current upstream checks.
type Header struct {
	Name  string
	Value string
}

// Resolved is a Profile after preset merge: the concrete identity a transport
// applies.
type Resolved struct {
	Preset     string
	UserAgent  string
	Headers    []Header
	TLSProfile string
}

// Resolve merges p with PresetDefault.
func (p Profile) Resolve() Resolved { return p.ResolveWith(PresetDefault) }

// ResolveWith merges p on top of def, which is the provider's own historical
// default (empty = honest omnigate UA; newapi passes PresetBrowser).
//
// Precedence, lowest to highest: def preset → p.Preset → p.UserAgent and
// p.Headers → p.TLSProfile.
func (p Profile) ResolveWith(def string) Resolved {
	p = p.Normalized()
	presetName := p.Preset
	if presetName == "" {
		presetName = def
	}
	base, ok := presets[presetName]
	if !ok {
		// Unknown preset (hand-edited config): fall back rather than emit a
		// half-built identity.
		base, ok = presets[PresetDefault]
		if !ok {
			base = Resolved{Preset: PresetDefault, UserAgent: "omnigate/" + Version}
		}
	}
	base.Preset = presetName

	out := Resolved{
		Preset:     presetName,
		UserAgent:  base.UserAgent,
		Headers:    append([]Header(nil), base.Headers...),
		TLSProfile: base.TLSProfile,
	}
	if p.UserAgent != "" {
		out.UserAgent = p.UserAgent
	}
	if p.TLSProfile != "" {
		if p.TLSProfile == "none" {
			out.TLSProfile = ""
		} else {
			out.TLSProfile = p.TLSProfile
		}
	}
	if len(p.Headers) > 0 {
		out.Headers = mergeHeaders(out.Headers, p.Headers)
	}
	return out
}

// mergeHeaders applies overrides onto an ordered header list: an existing name is
// replaced in place (keeping its position), a new name is appended in sorted
// order so repeat calls are stable.
func mergeHeaders(base []Header, over map[string]string) []Header {
	seen := make(map[string]int, len(base))
	for i, h := range base {
		seen[http.CanonicalHeaderKey(h.Name)] = i
	}
	extra := make([]string, 0, len(over))
	for k := range over {
		if _, ok := seen[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	out := append([]Header(nil), base...)
	for k, v := range over {
		if i, ok := seen[k]; ok {
			out[i].Value = v
		}
	}
	for _, k := range extra {
		out = append(out, Header{Name: k, Value: over[k]})
	}
	return out
}

// Apply writes the identity onto req. The User-Agent is always set (never left to
// Go's default), then each header in order. Callers that need to add more headers
// should call Apply first so explicit provider headers win.
func (r Resolved) Apply(req *http.Request) {
	if req == nil {
		return
	}
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	for _, h := range r.Headers {
		if strings.EqualFold(h.Name, "host") || strings.EqualFold(h.Name, "content-length") {
			// net/http owns these; setting them corrupts the request.
			continue
		}
		req.Header.Set(h.Name, h.Value)
	}
}

// Header returns the resolved value for name ("" when absent), for diagnostics
// and tests.
func (r Resolved) Header(name string) string {
	if strings.EqualFold(name, "user-agent") {
		return r.UserAgent
	}
	for _, h := range r.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// presets is the static identity table. Keep values believable: an upstream that
// validates client shape (relay anti-abuse) compares against the real clients.
var presets = map[string]Resolved{
	PresetDefault: {
		Preset:    PresetDefault,
		UserAgent: "omnigate/" + Version,
	},
	// PresetHonest is the same identity under the panel-visible name; it is a
	// real entry rather than a fallback so an operator choosing "如实申报" gets a
	// deterministic result even after the preset table grows.
	PresetHonest: {
		Preset:    PresetHonest,
		UserAgent: "omnigate/" + Version,
	},
	PresetBrowser: {
		Preset:    PresetBrowser,
		UserAgent: ChromeUA,
		Headers: []Header{
			{Name: "Accept-Language", Value: AcceptLanguage},
			{Name: "Accept", Value: "application/json, text/plain, */*"},
			{Name: "Sec-Fetch-Site", Value: "same-origin"},
			{Name: "Sec-Fetch-Mode", Value: "cors"},
			{Name: "Sec-Fetch-Dest", Value: "empty"},
		},
	},
	PresetClaudeCode: {
		Preset:     PresetClaudeCode,
		UserAgent:  "claude-cli/" + ClaudeCodeVersion + " (external, cli)",
		TLSProfile: "chrome120",
		Headers: []Header{
			{Name: "x-app", Value: "cli"},
			{Name: "anthropic-version", Value: "2023-06-01"},
			{Name: "anthropic-beta", Value: "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"},
			{Name: "Accept", Value: "application/json"},
			{Name: "x-stainless-lang", Value: "js"},
			{Name: "x-stainless-package-version", Value: "0.68.0"},
			{Name: "x-stainless-os", Value: "MacOS"},
			{Name: "x-stainless-arch", Value: "arm64"},
			{Name: "x-stainless-runtime", Value: "node"},
			{Name: "x-stainless-runtime-version", Value: "v22.14.0"},
			{Name: "x-stainless-retry-count", Value: "0"},
			{Name: "x-stainless-timeout", Value: "600"},
		},
	},
	PresetCodex: {
		Preset:     PresetCodex,
		UserAgent:  "codex_cli_rs/" + CodexVersion + " (Windows 11; x86_64) WindowsTerminal",
		TLSProfile: "chrome120",
		Headers: []Header{
			{Name: "originator", Value: "codex_cli_rs"},
			{Name: "version", Value: CodexVersion},
			{Name: "OpenAI-Beta", Value: "responses=experimental"},
			{Name: "Accept", Value: "text/event-stream"},
		},
	},
}
