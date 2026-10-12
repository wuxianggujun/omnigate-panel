package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
)

// ChatRequest is a Gemini generateContent request translated to a
// chat/completions request, plus the bookkeeping the caller needs.
type ChatRequest struct {
	// Body is the marshalled chat/completions request body.
	Body []byte
	// Stream reports whether the client called streamGenerateContent.
	Stream bool
	// Model is the model name with any "models/" prefix stripped.
	Model string
	// InputTokens is a local estimate for usageMetadata.promptTokenCount. It is a
	// rough proxy (see estimateTokens), not a tokenizer count.
	InputTokens int
	// ImageIntent reports that the last user turn is a plain image-generation
	// request, in which case the client's tool declarations were dropped. See
	// intent.go for why.
	ImageIntent bool
	// DroppedTools counts tool declarations that did not survive translation
	// (no name), plus those dropped for ImageIntent. The caller logs it: tools
	// that vanish silently are otherwise only discovered through a user report.
	DroppedTools int
}

// RequestToChat translates a Gemini generateContent body into a chat/completions
// request. model is the path segment of /v1beta/models/{model}:generateContent;
// a leading "models/" is stripped (the official SDK includes it).
func RequestToChat(model string, raw []byte, stream bool) (*ChatRequest, error) {
	model = strings.TrimPrefix(model, "models/")

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	contents, err := parseContents(rawAlt(top, "contents"))
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("contents is required")
	}

	var msgs []openai.Message
	if sys := systemText(top); sys != "" {
		msgs = append(msgs, openai.Message{Role: "system", Content: openai.Content{Text: sys}})
	}
	// Gemini has no tool-call ids; OpenAI's tool result messages need one. Match
	// each functionResponse back to the id minted for the functionCall of the same
	// name so a native-tool provider sees a well-formed conversation.
	callIDs := map[string]string{}
	for _, c := range contents {
		msgs = append(msgs, contentToMessages(c, callIDs)...)
	}

	// 生图意图必须在未拍平的 contents 上判断：拍平之后 functionResponse 已被渲染
	// 成 "[Tool result: …]" 文本，字符串层面分不清「用户真在要图」和「工具结果里
	// 提到了图」，误判会静默丢掉客户端声明的 tools。
	intent := isImageGenerationIntent(lastUserText(contents))

	tools, dropped := parseTools(top)
	if intent {
		dropped += len(tools)
		tools = nil
	}

	req := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   stream,
	}
	if len(tools) > 0 {
		req["tools"] = tools
	}
	for k, v := range generationConfig(top) {
		req[k] = v
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return &ChatRequest{
		Body:         body,
		Stream:       stream,
		Model:        model,
		InputTokens:  estimateTokens(raw),
		ImageIntent:  intent,
		DroppedTools: dropped,
	}, nil
}

// rawAlt returns the first present value among keys, so a request written in
// either the official camelCase wire format or snake_case is accepted (upstream
// gets this from pydantic's alias generator).
func rawAlt(m map[string]json.RawMessage, keys ...string) json.RawMessage {
	for _, k := range keys {
		if v, ok := m[k]; ok && len(v) > 0 {
			return v
		}
	}
	return nil
}

// geminiContent is one entry of contents[]: a role plus parts.
type geminiContent struct {
	Role  string            `json:"role"`
	Parts []json.RawMessage `json:"parts"`
}

func parseContents(raw json.RawMessage) ([]geminiContent, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []geminiContent
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("invalid contents: %w", err)
	}
	return out, nil
}

// systemText flattens systemInstruction, which the API accepts either as a plain
// string or as a {parts:[{text}]} content object.
func systemText(top map[string]json.RawMessage) string {
	raw := rawAlt(top, "systemInstruction", "system_instruction")
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var c geminiContent
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	var texts []string
	for _, p := range c.Parts {
		if t := partText(p); t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, " ")
}

// lastUserText returns the text of the last role=="user" content, considering
// only text parts. It deliberately stops at that message even when its text is
// empty (e.g. it carried only a functionResponse) rather than falling back to an
// earlier user turn — same semantics as upstream's _last_user_text_native.
func lastUserText(contents []geminiContent) string {
	for i := len(contents) - 1; i >= 0; i-- {
		if contents[i].Role != "user" {
			continue
		}
		var texts []string
		for _, p := range contents[i].Parts {
			if t := partText(p); t != "" {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, " ")
	}
	return ""
}

// partText extracts a part's text, accepting either spelling of the key.
func partText(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	v := rawAlt(m, "text")
	if len(v) == 0 {
		return ""
	}
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

// contentToMessages translates one contents[] entry into chat messages. A
// functionCall becomes an assistant tool_calls entry and a functionResponse a
// role="tool" message, so a provider with native tool support (and the prompt
// emulation used by prompt-based providers) both see the tool loop.
func contentToMessages(c geminiContent, callIDs map[string]string) []openai.Message {
	role := c.Role
	switch role {
	case "model":
		role = "assistant"
	case "":
		role = "user"
	}

	var texts []string
	var calls []openai.ToolCall
	var results []openai.Message

	for _, p := range c.Parts {
		if t := partText(p); t != "" {
			texts = append(texts, t)
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(p, &m) != nil {
			continue
		}
		if raw := rawAlt(m, "functionCall", "function_call"); len(raw) > 0 {
			if tc, ok := parseFunctionCall(raw); ok {
				callIDs[tc.Function.Name] = tc.ID
				calls = append(calls, tc)
			}
			continue
		}
		if raw := rawAlt(m, "functionResponse", "function_response"); len(raw) > 0 {
			name, text := parseFunctionResponse(raw)
			id := callIDs[name]
			if id == "" {
				id = name
			}
			results = append(results, openai.Message{
				Role:       "tool",
				Name:       name,
				ToolCallID: id,
				Content:    openai.Content{Text: text},
			})
			continue
		}
		if raw := rawAlt(m, "inlineData", "inline_data"); len(raw) > 0 {
			// The chat layer this pipeline reuses is text-only (openai.Content
			// drops every non-text part), so an attachment cannot be forwarded as
			// an image_url. Degrade to a marker instead of silently dropping it —
			// the same call the Anthropic layer makes for images inside a
			// tool_result block.
			texts = append(texts, "[image omitted]")
			continue
		}
		if raw := rawAlt(m, "fileData", "file_data"); len(raw) > 0 {
			texts = append(texts, "[file omitted]")
			continue
		}
	}

	var out []openai.Message
	if len(calls) > 0 {
		out = append(out, openai.Message{
			Role:      "assistant",
			Content:   openai.Content{Text: strings.Join(texts, " ")},
			ToolCalls: calls,
		})
	} else if len(texts) > 0 {
		out = append(out, openai.Message{Role: role, Content: openai.Content{Text: strings.Join(texts, " ")}})
	}
	out = append(out, results...)
	return out
}

// parseFunctionCall translates a Gemini functionCall part into an OpenAI tool
// call, minting an id (Gemini does not carry one).
func parseFunctionCall(raw json.RawMessage) (openai.ToolCall, bool) {
	var fc struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	if json.Unmarshal(raw, &fc) != nil || fc.Name == "" {
		return openai.ToolCall{}, false
	}
	args := strings.TrimSpace(string(fc.Args))
	if args == "" || args == "null" {
		args = "{}"
	}
	return openai.ToolCall{
		ID:       util.NewCallID(),
		Type:     "function",
		Function: openai.ToolFunction{Name: fc.Name, Arguments: args},
	}, true
}

// parseFunctionResponse flattens a Gemini functionResponse part into the (name,
// text) pair a chat tool message needs. response may be a string or an object.
func parseFunctionResponse(raw json.RawMessage) (string, string) {
	var fr struct {
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	}
	_ = json.Unmarshal(raw, &fr)
	text := strings.TrimSpace(string(fr.Response))
	if text == "" || text == "null" {
		return fr.Name, ""
	}
	if text[0] == '"' {
		var s string
		if json.Unmarshal(fr.Response, &s) == nil {
			return fr.Name, s
		}
	}
	return fr.Name, text
}

// parseTools translates tools[].functionDeclarations[] into OpenAI tool
// definitions. The second return is the number of declarations dropped because
// they carried no name (a nameless function is invisible to the model).
func parseTools(top map[string]json.RawMessage) ([]openai.Tool, int) {
	raw := rawAlt(top, "tools")
	if len(raw) == 0 {
		return nil, 0
	}
	var defs []map[string]json.RawMessage
	if json.Unmarshal(raw, &defs) != nil {
		return nil, 0
	}
	var out []openai.Tool
	dropped := 0
	for _, d := range defs {
		decls := rawAlt(d, "functionDeclarations", "function_declarations")
		if len(decls) == 0 {
			continue
		}
		var list []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(decls, &list) != nil {
			continue
		}
		for _, fd := range list {
			if fd.Name == "" {
				dropped++
				continue
			}
			params := fd.Parameters
			if len(params) == 0 || string(params) == "null" {
				// The upstream only treats an entry as callable when it carries a
				// parameters schema; a declaration without one would be invisible.
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			out = append(out, openai.Tool{
				Type: "function",
				Function: openai.ToolDef{
					Name:        fd.Name,
					Description: fd.Description,
					Parameters:  params,
				},
			})
		}
	}
	return out, dropped
}

// generationConfig maps generationConfig onto the chat sampling fields.
func generationConfig(top map[string]json.RawMessage) map[string]any {
	raw := rawAlt(top, "generationConfig", "generation_config")
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := map[string]any{}
	for _, kv := range []struct {
		dst  string
		keys []string
	}{
		{"temperature", []string{"temperature"}},
		{"max_tokens", []string{"maxOutputTokens", "max_output_tokens"}},
		{"top_p", []string{"topP", "top_p"}},
	} {
		v := rawAlt(m, kv.keys...)
		if len(v) == 0 {
			continue
		}
		var anyv any
		if json.Unmarshal(v, &anyv) == nil {
			out[kv.dst] = anyv
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// estimateTokens is the same rough proxy the upstream uses (4 bytes per token).
// It is only ever reported back as usageMetadata; it is not used for routing.
func estimateTokens(b []byte) int { return len(b) / 4 }
