package gemini

import (
	"encoding/json"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// chatUsage is the chat/completions usage object.
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatResponse is the subset of a chat/completions response this package reads.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string           `json:"content"`
			ToolCalls []openai.ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

func parseChatResponse(body []byte) (*chatResponse, error) {
	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, err
	}
	return &cr, nil
}

// ChatResponseToGemini translates a non-streaming chat/completions response into
// a Gemini generateContent response. inputTokens is the local request estimate,
// used only when the upstream reported no usage.
func ChatResponseToGemini(body []byte, inputTokens int) ([]byte, error) {
	cr, err := parseChatResponse(body)
	if err != nil {
		return nil, err
	}
	var content string
	var calls []openai.ToolCall
	var finish string
	if len(cr.Choices) > 0 {
		content = cr.Choices[0].Message.Content
		calls = cr.Choices[0].Message.ToolCalls
		finish = cr.Choices[0].FinishReason
	}

	prompt := inputTokens
	completion := estimateTokens([]byte(content))
	if cr.Usage != nil {
		if cr.Usage.PromptTokens > 0 {
			prompt = cr.Usage.PromptTokens
		}
		if cr.Usage.CompletionTokens > 0 {
			completion = cr.Usage.CompletionTokens
		}
	}

	out := map[string]any{
		"candidates": []map[string]any{{
			"content":      map[string]any{"parts": responseParts(content, calls), "role": "model"},
			"finishReason": geminiFinishReason(finish),
			"index":        0,
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     prompt,
			"candidatesTokenCount": completion,
			"totalTokenCount":      prompt + completion,
		},
	}
	return json.Marshal(out)
}

// responseParts renders the reply as Gemini parts. A tool call wins over text
// (the same rule the upstream applies: when the model called a tool it does not
// also emit prose).
func responseParts(content string, calls []openai.ToolCall) []map[string]any {
	if len(calls) > 0 {
		parts := make([]map[string]any, 0, len(calls))
		for _, tc := range calls {
			parts = append(parts, functionCallPart(tc))
		}
		return parts
	}
	return []map[string]any{{"text": content}}
}

// functionCallPart renders one tool call as a Gemini functionCall part. args must
// be an object on the wire; a fragment that does not parse is handed over as a
// string so the caller still sees what the model produced.
func functionCallPart(tc openai.ToolCall) map[string]any {
	var args any
	if s := strings.TrimSpace(tc.Function.Arguments); s != "" {
		if json.Unmarshal([]byte(s), &args) != nil {
			args = s
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	return map[string]any{
		"functionCall": map[string]any{"name": tc.Function.Name, "args": args},
	}
}

// geminiFinishReason maps a chat finish_reason onto Gemini's enum.
func geminiFinishReason(chat string) string {
	switch chat {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	default:
		return "STOP"
	}
}
