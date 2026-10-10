package upstream

import (
	"net/http"
	"testing"
)

// EffectiveEffortOf 读改写完成后出站 body 的实际思考档位：snake 优先、camel 兜底、
// 显式关闭回 "off"、无档位回空串。它是请求日志「实际思考程度」列的数据源。
func TestEffectiveEffortOf(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"snake 档位", `{"model":"deepseek-v4-flash","reasoning_effort":"high"}`, "high"},
		{"camel 档位", `{"model":"x","reasoningEffort":"medium"}`, "medium"},
		{"snake 优先于 camel", `{"reasoning_effort":"low","reasoningEffort":"high"}`, "low"},
		{"档位裁剪空白", `{"reasoning_effort":"  high  "}`, "high"},
		{"显式关闭 → off", `{"model":"deepseek-v4-flash","thinking":{"type":"disabled"}}`, "off"},
		{"无档位无 thinking → 空", `{"model":"glm-5.2"}`, ""},
		{"enabled 无档位 → 空", `{"thinking":{"type":"enabled"}}`, ""},
		{"空 body", ``, ""},
		{"坏 JSON", `{`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveEffortOf([]byte(tc.body)); got != tc.want {
				t.Errorf("EffectiveEffortOf(%s)=%q want %q", tc.body, got, tc.want)
			}
		})
	}
}

// deepseek 系裸请求经网关注入 thinking.enabled + 模型默认档后，EffectiveReasoningEffort
// 必须回报注入后的档位（与真正出站 body 同源），否则请求日志会误报「没思考」。
func TestEffectiveReasoningEffortDeepSeekDefault(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{}`), nil
	})
	// 只声明默认档（无 supportedEfforts）：deepseek 缺档时补模型声明默认档。
	c.storeEfforts("cn", nil, map[string]string{"deepseek-v4-flash": "medium"})
	if got := c.EffectiveReasoningEffort("cn", "deepseek-v4-flash", []byte(`{"model":"deepseek-v4-flash","messages":[]}`)); got != "medium" {
		t.Errorf("deepseek default effort=%q want medium", got)
	}
	// 非 deepseek 模型不注入档位 → 空串（未开思考）。
	if got := c.EffectiveReasoningEffort("cn", "glm-5.2", []byte(`{"model":"glm-5.2","messages":[]}`)); got != "" {
		t.Errorf("glm effort=%q want empty", got)
	}
}
