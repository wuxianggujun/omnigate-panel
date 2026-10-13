// provsec_html_test.go 校验 index.html 与 app.js 的分组 Tab 严格一一对应，
// 且旧供应商页的重复入口已清干净。
package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestProviderSectionTabsHTMLMatchesJS 钉住三件事：
//  1. HTML 的 tab 名与 JS 的 PROV_SECS 是同一套（改一个另一个得跟着改）；
//  2. 每个 tab 都有对应的 .cfgsec-sec 容器（缺一个 = 点进去一片空白）；
//  3. 旧供应商页那 3 个"保存并生效"按钮全部清掉（保存入口收敛成页脚 1 个），
//     且供应商页不再混用 .og-sub / .og-fields 那套竖排小标签。
func TestProviderSectionTabsHTMLMatchesJS(t *testing.T) {
	h, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	H, J := string(h), string(js)

	tabs := regexp.MustCompile(`(?s)<div class="cfgsec-tabs" id="provSecTabs">(.*?)</div>`).FindStringSubmatch(H)
	if tabs == nil {
		t.Fatal("index.html 缺少 #provSecTabs")
	}
	var htmlSecs []string
	for _, m := range regexp.MustCompile(`data-sec="([a-z]+)"`).FindAllStringSubmatch(tabs[1], -1) {
		htmlSecs = append(htmlSecs, m[1])
	}
	jsSecs := regexp.MustCompile(`const PROV_SECS = \[([^\]]+)\]`).FindStringSubmatch(J)
	if jsSecs == nil {
		t.Fatal("app.js 缺少 PROV_SECS")
	}
	var jsList []string
	for _, s := range strings.Split(jsSecs[1], ",") {
		s = strings.Trim(strings.TrimSpace(s), "'")
		if s != "" {
			jsList = append(jsList, s)
		}
	}
	if strings.Join(htmlSecs, ",") != strings.Join(jsList, ",") {
		t.Fatalf("分组名不一致: html=%v js=%v", htmlSecs, jsList)
	}
	if len(htmlSecs) != 4 {
		t.Fatalf("供应商页应有 4 组，实际 %d: %v", len(htmlSecs), htmlSecs)
	}

	for _, s := range htmlSecs {
		if !strings.Contains(H, `<div class="cfgsec-sec" data-sec="`+s+`"`) {
			t.Fatalf("index.html 缺少 .cfgsec-sec[data-sec=%s]", s)
		}
	}

	// 旧的重复保存按钮必须全部清掉。
	for _, id := range []string{"btnSaveCfg2", "btnSaveCfgBot", "btnSaveCfgCredit", "btnSaveCfgReliability"} {
		if strings.Contains(H, `id="`+id+`"`) {
			t.Fatalf("index.html 仍残留 %s：供应商页保存入口应只有 #btnSaveCfg 一个", id)
		}
		if strings.Contains(J, "'"+id+"'") {
			t.Fatalf("app.js 仍绑定 %s", id)
		}
	}
	if n := strings.Count(H, `id="btnSaveCfg"`); n != 1 {
		t.Fatalf("供应商页保存按钮应只有 1 个，实际 %d", n)
	}

	// 供应商页里不能再出现 .og-sub / .og-fields：那是"同一页三套表单写法"的源头。
	// 出站代理页仍在用（不属于本轮范围），所以只截供应商页这一段来查。
	provView := H[strings.Index(H, `id="view-providers"`):]
	if i := strings.Index(provView, `id="view-outbound"`); i > 0 {
		provView = provView[:i]
	}
	for _, bad := range []string{"og-sub", "og-fields"} {
		if strings.Contains(provView, bad) {
			t.Fatalf("供应商页仍含 .%s：表单写法应统一为 .grid*/.fld", bad)
		}
	}
}
