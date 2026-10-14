package panel

// modelpage_refactor_test.go 锁「模型与档位」页与「配置页 → 网关密钥」两处重构后的
// 结构。改的是纯前端（HTML/CSS/JS），但 Go 测试必须跟着锁——否则一次无心的删 id /
// 改类名，前端静默失效而 `go test` 一路绿灯。
//
// 本轮锁三件事：
//   1. 「逐模型优先供应商」列（rr-prov 表头 + .rr-pin 下拉）：把「模型可以单独设置」
//      落进模型页，写入口是 model_mapping（后端唯一载体），不新增第二套机制。
//   2. 密钥卡片的「这把 key 能调哪些模型」预览容器 .kscope：它治的是「改了路由忘改
//      scope、客户端永久 403 而界面上看不出原因」，不能悄悄消失。
//   3. 「网关密钥」卡的说明必须留在显眼位置（卡片顶部，不是埋在列表下面）——用户明确
//      要求「把说明挪到显眼位置」。

import (
	"os"
	"strings"
	"testing"
)

func readPanelFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 模型页「优先供应商」列：表头、下拉类名、以及 JS 侧对应的事件分支必须同时存在。
// 只有 HTML 没有事件分支 = 下拉能动但保存不了；只有 JS 没有表头 = 永远渲染不出来。
func TestModelPagePerModelProviderColumn(t *testing.T) {
	H := readPanelFile(t, "index.html")
	J := readPanelFile(t, "app.js")

	if !strings.Contains(H, `<th class="rr-prov">优先供应商</th>`) {
		t.Error("模型页缺「优先供应商」表头（th.rr-prov）——逐模型单独设置没有落脚点")
	}
	// 下拉的类名是 JS 事件委托的依据（select.rr-pin），改了这个选择器就静默失效。
	if n := strings.Count(J, `class="rr-pin"`); n != 1 {
		t.Errorf("app.js 里 select.rr-pin 出现 %d 次，want 1（pin 下拉的类名）", n)
	}
	if n := strings.Count(J, `select.rr-pin`); n < 1 {
		t.Error("app.js 缺 select.rr-pin 的事件委托选择器")
	}
	if n := strings.Count(J, `select.rr-sel`); n < 1 {
		t.Error("app.js 缺 select.rr-sel 的事件委托选择器（原有优先域下拉）")
	}
	// 写入口必须是 model_mapping，不能另起炉灶
	for _, want := range []string{
		`async function mmPinFromPicker`,
		`'/omni/model-mapping'`,
		`'/omni/model-mapping/' + encodeURIComponent(model)`,
	} {
		if !strings.Contains(J, want) {
			t.Errorf("app.js 缺 pin 写入口片段: %s", want)
		}
	}
	// 快照与读取守卫：pin 列读 omniMapping，加载走 canReadModelMap（与后端
	// privileged 中间件同口径：admin/viewer 可读，普通 user 403）
	for _, want := range []string{`function canReadModelMap`, `omniMapping`, `omniMappingOff`} {
		if !strings.Contains(J, want) {
			t.Errorf("app.js 缺 mapping 快照相关片段: %s", want)
		}
	}
	// 表从 3 列（模型/域/供应商）起，空态提示的 colspan 必须同步，否则空表时
	// 提示文字只占一列、排版塌掉。
	if !strings.Contains(J, `colspan="4"`) {
		t.Error("app.js 空态 colspan 未随 3 列 + 操作列更新（want colspan=\"4\"）")
	}
}

// 密钥卡片预览：容器类名 + 计算函数 + 三个必要提示。
func TestKeyScopePreviewPresent(t *testing.T) {
	J := readPanelFile(t, "app.js")
	C := readPanelFile(t, "theme.css")

	for _, want := range []string{
		`function keyScopeModels`,
		`function keyScopePreviewHtml`,
		`class="kscope"`,
	} {
		if !strings.Contains(J, want) {
			t.Errorf("app.js 缺 key scope 预览片段: %s", want)
		}
	}
	// 空态必须说清「为什么 0 个」，而不是只显示一个空框
	for _, want := range []string{
		`0 个模型可调`,
		`模型目录是空的`,
		`已经不存在`,
		`无论请求什么都会 403`,
	} {
		if !strings.Contains(J, want) {
			t.Errorf("app.js 的 key scope 空态缺提示文案: %s", want)
		}
	}
	// 样式必须存在，且只能使用 theme.css 里已定义的令牌（不许引入猜出来的变量）
	for _, want := range []string{`.kscope {`, `.kscope-bad {`} {
		if !strings.Contains(C, want) {
			t.Errorf("theme.css 缺 key scope 预览样式: %s", want)
		}
	}
	for _, bad := range []string{`var(--mut`, `var(--brand`, `var(--warn-ink`} {
		if strings.Contains(C, bad) {
			t.Errorf("theme.css 引用了不存在的令牌 %s（真实名称见 theme.css 顶部 :root）", bad)
		}
	}
}

// 「网关密钥」说明必须留在卡片顶部显眼处：关键词不能消失。
// 用户原话是「把说明挪到显眼位置」，缩回列表下方等于没做。
func TestGatewayKeyHintStaysProminent(t *testing.T) {
	H := readPanelFile(t, "index.html")
	i := strings.Index(H, `id="keyEditor"`)
	if i < 0 {
		t.Fatal("index.html 缺 #keyEditor")
	}
	// 说明块必须在 keyEditor 之前（同卡片的上方），而不是之后。
	// 匹配的文案要与 index.html 逐字一致（这里是 <strong>不勾任何供应商 = 全量</strong>）。
	for _, phrase := range []string{
		`<strong>不勾任何供应商 = 全量</strong>`,
		`<strong>勾选 = 收窄</strong>`,
		`一把密钥可以授权多家供应商`,
	} {
		j := strings.Index(H, phrase)
		if j < 0 {
			t.Errorf("index.html 缺网关密钥说明文案: %s", phrase)
			continue
		}
		if j > i {
			t.Errorf("「%s」在 #keyEditor 之后——说明又被埋回列表下面了", phrase)
		}
	}
	// 403 的实弹案例要留在说明里：它解释了预览为什么存在
	if !strings.Contains(H, `requested model routes to provider`) {
		t.Error("缺 403 实例说明——预览功能的存在理由没有被解释")
	}
	// 「实时算出能调哪些模型」这句话是预览功能的入口指引，不能丢
	if !strings.Contains(H, `每张卡片下方会实时算出「这把 key 到底能调哪些模型」`) {
		t.Error("缺预览功能指引文案")
	}
}
