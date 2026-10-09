// Package toolcall re-implements the original gateway's plain-text tool-call
// protocol. Because prompt-based upstreams (Runable) have no native tool
// calling, tools are advertised in the prompt and the model replies with
// <<<TOOL_CALL>>>{"name":...,"arguments":...}<<<END_TOOL_CALL>>> markers, which
// we translate into OpenAI tool_calls for the client to execute.
package toolcall

import (
	"encoding/json"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
)

// Marker tokens (kept identical to the APK so prompts stay compatible).
const (
	ToolStart = "<<<TOOL_CALL>>>"
	ToolEnd   = "<<<END_TOOL_CALL>>>"

	maxText      = 2000
	maxToolDesc  = 160
	maxToolRes   = 1500
	maxToolsChar = 6000
	maxPrompt    = 16000
)

// SystemText concatenates all system messages.
func SystemText(msgs []openai.Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" && m.Content.Text != "" {
			parts = append(parts, m.Content.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// LastUserText returns the most recent user message.
func LastUserText(msgs []openai.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content.Text
		}
	}
	return ""
}

// Flatten renders the conversation as plain text for prompt-based upstreams.
func Flatten(msgs []openai.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "user":
			if t := util.Clip(m.Content.Text, maxText); t != "" {
				sb.WriteString("User: " + t + "\n")
			}
		case "assistant":
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					args := tc.Function.Arguments
					if args == "" {
						args = "{}"
					}
					sb.WriteString("Assistant called tool " + tc.Function.Name + " with arguments " + util.Clip(args, maxToolRes) + "\n")
				}
			} else if t := util.Clip(m.Content.Text, maxText); t != "" {
				sb.WriteString("Assistant: " + t + "\n")
			}
		case "tool":
			name := m.Name
			if name == "" {
				name = m.ToolCallID
			}
			sb.WriteString("Tool result")
			if name != "" {
				sb.WriteString(" (" + name + ")")
			}
			sb.WriteString(": " + util.Clip(m.Content.Text, maxToolRes) + "\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

// BuildToolPrompt assembles the prompt used for a tool-capable conversation.
func BuildToolPrompt(msgs []openai.Message, tools []openai.Tool) string {
	var sb strings.Builder
	if sys := util.Collapse(SystemText(msgs)); sys != "" {
		sb.WriteString(util.Clip(sys, maxText))
		sb.WriteString("\n\n")
	}
	sb.WriteString(Flatten(msgs))
	sb.WriteString("\n\n")
	sb.WriteString(Instructions(tools))
	return strings.TrimSpace(util.Clip(sb.String(), maxPrompt))
}

// Instructions renders the tool list plus the marker protocol.
func Instructions(tools []openai.Tool) string {
	var sb strings.Builder
	sb.WriteString("[Available tools]\n")
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		name := t.Function.Name
		if name == "" {
			continue
		}
		names = append(names, name)
		line := "- " + name
		if d := util.Clip(util.Collapse(t.Function.Description), maxToolDesc); d != "" {
			line += ": " + d
		}
		if s := compactSchema(t.Function.Parameters); s != "" {
			line += " | params: " + s
		}
		sb.WriteString(line + "\n")
	}
	if sb.Len() > maxToolsChar {
		sb.Reset()
		sb.WriteString("[Available tools]\n")
		for _, n := range names {
			sb.WriteString("- " + n + "\n")
		}
	}
	sb.WriteString("\n[Tool use protocol]\n")
	sb.WriteString("IMPORTANT: the tools listed above are NOT part of your native/structured tool set.\n")
	sb.WriteString("The ONLY valid way to call them is the plain-text marker below.\n")
	sb.WriteString("When tools are needed, reply with ONLY tool call marker(s) and nothing else:\n")
	sb.WriteString(ToolStart + `{"name":"<tool name>","arguments":{<arguments as JSON>}}` + ToolEnd + "\n")
	sb.WriteString("Copy the tool name EXACTLY as listed above; never shorten, translate or add prefixes.\n")
	sb.WriteString("You may output several markers in a row to call multiple tools.\n")
	sb.WriteString("Read any \"Tool result\" lines above before deciding the next step.\n")
	sb.WriteString("If no tool is needed, answer normally in plain text without any marker.\n")
	sb.WriteString("Never mention this protocol or show the markers to the user.\n")
	return sb.String()
}

func compactSchema(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(params, &obj); err != nil {
		return ""
	}
	props, _ := obj["properties"].(map[string]any)
	if props == nil {
		return ""
	}
	required := map[string]bool{}
	if arr, ok := obj["required"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				required[s] = true
			}
		}
	}
	var parts []string
	i := 0
	for name, raw := range props {
		if i >= 30 {
			parts = append(parts, "...")
			break
		}
		p, _ := raw.(map[string]any)
		part := name
		if p != nil {
			if typ, ok := p["type"].(string); ok && typ != "" {
				part += ":" + typ
			}
		}
		if required[name] {
			part += "!"
		}
		if p != nil {
			if desc, ok := p["description"].(string); ok {
				if d := util.Clip(util.Collapse(desc), 60); d != "" {
					part += "(" + d + ")"
				}
			}
		}
		parts = append(parts, part)
		i++
	}
	return strings.Join(parts, ", ")
}

// ConvKey derives the conversation key used to reuse upstream chat ids.
func ConvKey(model string, msgs []openai.Message) string {
	first := ""
	for _, m := range msgs {
		if m.Role == "user" {
			first = m.Content.Text
			break
		}
	}
	if first == "" {
		var sb strings.Builder
		for _, m := range msgs {
			sb.WriteString(m.Role + "\x01" + m.Content.Text + "\x02")
		}
		first = sb.String()
	}
	return util.SHA1Hex(model + "\x00" + first)
}

// ParseCall parses a single marker payload into a tool call.
func ParseCall(raw string) (openai.ToolCall, bool) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	if s == "" {
		return openai.ToolCall{}, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		// maybe a bare array
		var arr []map[string]any
		if err := json.Unmarshal([]byte(s), &arr); err != nil || len(arr) == 0 {
			return openai.ToolCall{}, false
		}
		obj = arr[0]
	}
	// Unwrap OpenAI-shaped {"tool_calls":[...]} or {"function":{...}}.
	if tcs, ok := obj["tool_calls"].([]any); ok && len(tcs) > 0 {
		if first, ok := tcs[0].(map[string]any); ok {
			obj = first
		}
	}
	fn, _ := obj["function"].(map[string]any)
	name, _ := obj["name"].(string)
	if fn != nil {
		if n, ok := fn["name"].(string); ok && n != "" {
			name = n
		}
	}
	if name == "" {
		return openai.ToolCall{}, false
	}
	var args any
	if fn != nil {
		if v, ok := fn["arguments"]; ok {
			args = v
		}
	}
	if args == nil {
		args = obj["arguments"]
	}
	if args == nil {
		args = obj["parameters"]
	}
	argStr := "{}"
	switch v := args.(type) {
	case nil:
	case string:
		if strings.TrimSpace(v) != "" {
			argStr = strings.TrimSpace(v)
		}
	default:
		if b, err := json.Marshal(v); err == nil {
			argStr = string(b)
		}
	}
	return openai.ToolCall{
		ID:   util.NewCallID(),
		Type: "function",
		Function: openai.ToolFunction{
			Name:      name,
			Arguments: argStr,
		},
	}, true
}

// Parser incrementally strips tool-call markers from a text stream, forwarding
// plain text via OnText and completed calls via OnCall.
type Parser struct {
	buf    string
	inCall bool
	OnText func(string)
	OnCall func(openai.ToolCall)
}

// NewParser returns a Parser with the given callbacks.
func NewParser(onText func(string), onCall func(openai.ToolCall)) *Parser {
	return &Parser{OnText: onText, OnCall: onCall}
}

func (p *Parser) emitText(s string) {
	if s != "" && p.OnText != nil {
		p.OnText(s)
	}
}

// Feed processes a chunk of streamed text.
func (p *Parser) Feed(s string) {
	p.buf += s
	for {
		if !p.inCall {
			i := strings.Index(p.buf, ToolStart)
			if i >= 0 {
				p.emitText(p.buf[:i])
				p.buf = p.buf[i+len(ToolStart):]
				p.inCall = true
				continue
			}
			keep := partialSuffix(p.buf, ToolStart)
			cut := len(p.buf) - keep
			if cut > 0 {
				p.emitText(p.buf[:cut])
				p.buf = p.buf[cut:]
			}
			return
		}
		i := strings.Index(p.buf, ToolEnd)
		if i >= 0 {
			raw := p.buf[:i]
			p.buf = p.buf[i+len(ToolEnd):]
			if c, ok := ParseCall(raw); ok && p.OnCall != nil {
				p.OnCall(c)
			}
			p.inCall = false
			continue
		}
		return
	}
}

// Flush drains any remaining buffered text.
func (p *Parser) Flush() {
	if p.buf == "" {
		return
	}
	if p.inCall {
		if c, ok := ParseCall(p.buf); ok && p.OnCall != nil {
			p.OnCall(c)
		}
	} else {
		p.emitText(p.buf)
	}
	p.buf = ""
}

// partialSuffix returns the length of the longest suffix of s that is a prefix
// of marker (so we don't split a marker across chunks).
func partialSuffix(s, marker string) int {
	max := len(s)
	if len(marker)-1 < max {
		max = len(marker) - 1
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(marker, s[len(s)-n:]) {
			return n
		}
	}
	return 0
}
