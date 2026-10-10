package upstream

import "testing"

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

// prepareBody 必须回填与出站 body 同源的思考档位：deepseek 裸请求补默认档、
// 显式关闭记 off、非推理模型空串。这是请求日志「实际思考程度」列的数据来源。
func TestPrepareBodyEffortOut(t *testing.T) {
	c := testClient(nil)
	// 只声明默认档（无 supportedEfforts）：deepseek 缺档时补模型声明默认档。
	c.storeEfforts("cn", nil, map[string]string{"deepseek-v4-flash": "medium"})
	cases := []struct {
		name string
		body string
		want string
	}{
		{"deepseek 裸请求补默认档", `{"model":"deepseek-v4-flash","messages":[]}`, "medium"},
		{"deepseek 显式关闭 → off", `{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[]}`, "off"},
		{"非推理模型 → 空", `{"model":"glm-5.2","messages":[]}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, effort := c.prepareBody([]byte(tc.body), "cn", "u1", ""); effort != tc.want {
				t.Errorf("prepareBody effort=%q want %q (body=%s)", effort, tc.want, tc.body)
			}
		})
	}
}
