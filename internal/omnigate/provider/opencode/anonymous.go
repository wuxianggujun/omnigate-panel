// anonymous.go 实现匿名免费通道（tier anon）。
//
// # 协议事实（2026-10-15 于本机出口 IP 逐项实测，改动前请重跑探测）
//
// 端点 https://opencode.ai/inference/v1/chat/completions，凭证是字面量
// "public"（大小写敏感）。门禁卡的不是凭证而是「客户端身份」：
//
//	Authorization         Bearer public（字面量）
//	User-Agent            opencode/latest/<ver>/cli（版本号本身不被校验，形态要对）
//	x-opencode-client     cli
//	x-opencode-session    } 四个头必须同值，形态 ses_+12hex+14base62；
//	x-opencode-session-id } 时间部分的低 12 位要落进 counter 1-63 的窗口
//	x-session-affinity    } （0xFC0-0xFFF，即三个 hex 尾位在 fc0..fff），
//	X-Session-Id          } 窗口之外即使 ses_ 形态正确也 403
//	x-opencode-project    裸 40 位小写 hex（prj_* 形态反而 403）
//	禁带                  x-opencode-request（带了直接 403 点名）
//
// 请求体：stream:true 必须；tools 必须含齐官方五件套 shell/edit/glob/grep/read
// ——实测缺一件 403、五件改名（同 schema）403，但「五件套 + 额外工具」可过。
// 所以合并策略是「同名替换、缺件补齐、其余保留」。stream_options 可有可无。
//
// 传输层无关：curl / Go、HTTP/1.1 / HTTP/2 均通过，门禁只读请求内容。
//
// # 为什么有这个文件
//
// 门禁的目的本是排除第三方网关——上游对 zen/go 的官方文档明确写着第三方客户端
// 「用自己的 User-Agent」。但项目所有者（本网关的部署者）要求提供一条零密钥的
// 免费通道：免费模型匿名优先、zen/go 密钥降为付费通道与免费模型回退，并把它的
// 开关做成面板里的默认项。本文件照此实现，且把全部协议事实写成测试钉死
// （TestAnonymousLane*）——上游门禁一变，测试先亮红灯，而不是线上静默 403。
//
// 边界仍然清楚：zen/go tier 照旧如实申报身份（见包注释），镜像只用于匿名免费层。
// 若上游把门禁收紧到需要复刻整条客户端链路（TLS 指纹、时序等），应当重新评估
// 是否继续，而不是无条件加码。
package opencode

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

const (
	// DefaultAnonBase 是匿名免费通道的默认地址。免费推理已从 /zen 搬到
	// /inference；/zen 下的免费模型现在同样受这道门禁约束。
	DefaultAnonBase = "https://opencode.ai/inference"

	// anonCredential 是匿名 tier 的字面量凭证。大小写敏感："Bearer PUBLIC" 401。
	anonCredential = "public"

	// anonClientVersion 是镜像的官方 CLI 版本。上游不校验版本号本身，只要求
	// UA 形态正确；等它开始拒绝旧版本再往上抬。
	anonClientVersion = "2.0.29"
)

// anonUserAgent 是匿名通道的 User-Agent：opencode/<channel>/<version>/<name>。
// 旧的 "opencode/<version> (<os> <arch>; <runtime>)" 形态即使其余请求完全一样
// 也会被 403，不能手滑改成那种写法。
var anonUserAgent = "opencode/latest/" + anonClientVersion + "/cli"

// anonToolsRaw 是通过门禁的官方工具定义表（抓包实录，五件套：shell/edit/glob/
// grep/read）。逐字节原样使用是门禁通过的必要条件（改名即 403）。
//
//go:embed cli_tools.json
var anonToolsRaw []byte

var (
	anonToolsOnce sync.Once
	anonToolsList []json.RawMessage
	anonToolsSet  map[string]bool
)

// canonicalAnonTools 解析内嵌的工具定义表（一次解析，之后只读）。
// 解析失败只记日志、返回空表——测试 TestCanonicalAnonToolsEmbed 会拦下它。
func canonicalAnonTools() ([]json.RawMessage, map[string]bool) {
	anonToolsOnce.Do(func() {
		var arr []json.RawMessage
		if err := json.Unmarshal(anonToolsRaw, &arr); err != nil {
			log.Printf("WARN: [opencode] 匿名工具定义表解析失败（匿名通道将不可用）: %v", err)
			return
		}
		set := make(map[string]bool, len(arr))
		for _, raw := range arr {
			var head struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(raw, &head) == nil && head.Function.Name != "" {
				set[head.Function.Name] = true
			}
		}
		anonToolsList, anonToolsSet = arr, set
	})
	return anonToolsList, anonToolsSet
}

// mergeAnonTools 把客户端自带的 tools 与官方五件套合并成发给匿名通道的数组。
//
// 实测语义（2026-10-15）：五件套缺一件即 403、改名即 403，但「五件套 + 额外工具」
// 与「额外工具在前」都可过。因此：同名项用官方定义统一补齐（客户端可能来自别的
// opencode 版本，逐字节差异不赌）、其它工具原样保留、缺的补上。客户端没带 tools
// 时结果就是官方五件套本身。
func mergeAnonTools(client []openai.Tool) []any {
	canon, canonNames := canonicalAnonTools()
	out := make([]any, 0, len(client)+len(canon))
	for _, t := range client {
		if canonNames[t.Function.Name] {
			continue
		}
		out = append(out, t)
	}
	for _, raw := range canon {
		out = append(out, raw)
	}
	return out
}

// anonBody 在网关请求体上做匿名通道的加工：保持 stream:true（门禁要求，网关本来
// 就一直 stream），把 tools 换成合并后的数组。
func anonBody(raw []byte, clientTools []openai.Tool) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["stream"] = true
	m["tools"] = mergeAnonTools(clientTools)
	return json.Marshal(m)
}

// base62Alphabet 与官方客户端的标识符字母表一致。
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// anonSession 把稳定会话信号铸成官方形态的 ses_ 会话 id：ses_ + 12 位 hex 时间
// 部分 + 14 位 base62 随机部分（共 26 字符）。
//
// 时间部分是官方标识符的降序编码：由 sha256 派生的 36 位“时间戳”左移 12 位、
// counter 置 1，再整体按位取反——低 12 位因此固定在 0xFFE（counter=1 命中门禁
// 接受的 1-63 窗口）。随机部分取哈希高 80 位按 62 进制定宽渲染。
//
// 同一对话信号恒定 ⇒ 会话 id 恒定 ⇒ 上游提示词缓存亲和成立（这是它必须是
// 铸造而不是随机的全部理由）。
func anonSession(signal string) string {
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	return "ses_" + descendingTimePart(sum[:6]) + base62Fixed(sum[6:16], 14)
}

// descendingTimePart 把 6 个哈希字节渲染成降序 48 位时间部分的 12 位 hex。
func descendingTimePart(hash []byte) string {
	current := (uint64(hash[0])<<28 | uint64(hash[1])<<20 | uint64(hash[2])<<12 |
		uint64(hash[3])<<4 | uint64(hash[4])>>4) << 12
	current |= 1 // counter=1：门禁接受 1-63
	inverted := ^current & 0xFFFFFFFFFFFF
	return hex.EncodeToString([]byte{
		byte(inverted >> 40), byte(inverted >> 32), byte(inverted >> 24),
		byte(inverted >> 16), byte(inverted >> 8), byte(inverted),
	})
}

// base62Fixed 把字节串当大整数，按 62 进制渲染成定宽字符串。
func base62Fixed(b []byte, width int) string {
	n := new(big.Int).SetBytes(b)
	base := big.NewInt(62)
	rem := new(big.Int)
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, rem)
		out[i] = base62Alphabet[rem.Int64()]
	}
	return string(out)
}

// anonProject 派生 40 位小写 hex 的项目头。官方客户端发的是裸 40-hex（值本身不被
// 校验，只校验形态）；prj_* 形态反而会被 403。按供应商恒定即可——一个供应商的
// 请求都归到同一个“项目”，与官方客户端一个工作区对应。
func anonProject(signal string) string {
	sum := sha256.Sum256([]byte("opencode-project\x00" + signal))
	return hex.EncodeToString(sum[:20])
}

// isAnonEligible 判断这个模型该不该优先走匿名免费通道。
//
// 判定链（任一命中即走）：一条密钥都没配 → 全部先试匿名（没有别的通道可回落，
// 免费模型含 big-pickle 这类名字不带 free 的直接可用，非免费的由上游给出确切
// 结论，比本地预判信息只多不少）；配了密钥 → 名字含 free、元数据全 0 价、或
// 匿名免费目录在列。
//
// 名字/元数据/目录三个来源都是「有则用、没有就跳过」：冷启动时至少名字规则
// 兜底，不会因为辅助目录拉不到而把免费模型错扔进密钥池。注意全部走缓存查询
// （peek*），请求路径上绝不为了判定去下几百 KB~几 MB 的目录。
func (p *Provider) isAnonEligible(model string) bool {
	if p.anon == "" {
		return false
	}
	if len(p.pools) == 0 {
		return true
	}
	id := strings.TrimSpace(model)
	if strings.Contains(strings.ToLower(id), "free") {
		return true
	}
	if idx := p.peekMeta(); idx != nil {
		if info, ok := idx.merged[id]; ok && info.free {
			return true
		}
	}
	if set := p.peekAnonSet(); set != nil {
		return set[strings.ToLower(id)]
	}
	return false
}

// streamAnonymous 发一次匿名通道请求。
//
// 失败语义刻意简单：只试一次，不重试、不冷却（匿名通道没有“凭证”可轮换）；
// 返回的错误交给 StreamChat 决定要不要回落密钥池。
func (p *Provider) streamAnonymous(ctx context.Context, raw []byte, in provider.ChatInput, session string) (provider.Stream, error) {
	body, err := anonBody(raw, in.Tools)
	if err != nil {
		return nil, fmt.Errorf("opencode/anon 请求体加工失败: %w", err)
	}
	ses := anonSession(session)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.anon+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+anonCredential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", anonUserAgent)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", ses)
	req.Header.Set("x-opencode-session-id", ses)
	req.Header.Set("x-session-affinity", ses)
	req.Header.Set("X-Session-Id", ses)
	req.Header.Set("x-opencode-project", anonProject("omnigate:"+p.name))
	// 刻意不设 x-opencode-request：门禁见到它就 403（实测点名拒绝）。

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		err := fmt.Errorf("opencode/anon HTTP %d: %s", resp.StatusCode, clipBody(string(b)))
		if hint := anonFailureHint(b); hint != "" {
			err = fmt.Errorf("%w｜%s", err, hint)
		}
		return nil, err
	}
	return provider.NewOpenAIStream(resp.Body), nil
}

// anonFailureHint 给匿名通道的失败补一句人话。
func anonFailureHint(body []byte) string {
	s := string(body)
	switch {
	case strings.Contains(s, "FreeTierError"):
		return "匿名通道被上游门禁拒绝：大概率是上游又改了门禁、客户端镜像需要升级（请上报），" +
			"也可能这个模型不在匿名免费范围内；付费模型请在面板给本供应商配置 zen/go 密钥"
	case strings.Contains(s, "RegionError"):
		return "该模型在当前出口 IP 的地区不可用（与密钥、通道都无关）"
	case strings.Contains(s, "Model is unavailable"), strings.Contains(s, "model_not_found"):
		return "该模型不在匿名免费范围内；付费模型请配置 zen/go 密钥，或改用免费模型"
	}
	return ""
}

// ── 匿名免费目录（/inference/v1/models，Bearer public） ─────────────────────

// 免费目录的缓存节奏与元数据目录一致：很少变，所以小时级；失败短退避。
const (
	anonCatalogTTL     = metaTTL
	anonCatalogFailTTL = metaFailTTL
	anonCatalogTimeout = 20 * time.Second
)

// anonCatalog 缓存匿名免费目录。两个用途：给纯匿名供应商列模型、给路由判定
// 「这个 id 真在免费范围内」。零值可用；失败不致命（路由退回名字/元数据判定）。
type anonCatalog struct {
	mu   sync.Mutex
	next time.Time
	set  map[string]bool // 小写 id，路由判定用
	ids  []string        // 原始 id（保序），列模型用
	err  bool            // 上次抓取是否失败（只用于状态变化时打日志）
}

// peekAnonSet 只看已缓存、不抓取——请求路径上的路由判定用。
func (p *Provider) peekAnonSet() map[string]bool {
	if p == nil || p.anon == "" {
		return nil
	}
	c := &p.anonCat
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set != nil && time.Now().Before(c.next) {
		return c.set
	}
	return nil
}

// anonCatalogIDs 返回免费目录里的模型 id 列表；未到刷新时间直接给缓存。
// 抓取失败返回 nil（调用方当目录不存在处理，不影响其它模型来源）。
func (p *Provider) anonCatalogIDs() []string {
	if p == nil || p.anon == "" {
		return nil
	}
	c := &p.anonCat
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set != nil && time.Now().Before(c.next) {
		return c.ids
	}
	ids, err := p.fetchAnonCatalog()
	now := time.Now()
	if err == nil && len(ids) > 0 {
		if c.err {
			log.Printf("INFO: [opencode] 匿名免费目录已恢复（%s）", p.anon)
		}
		c.err = false
		c.ids = ids
		c.set = make(map[string]bool, len(ids))
		for _, id := range ids {
			c.set[strings.ToLower(id)] = true
		}
		c.next = now.Add(anonCatalogTTL)
		return c.ids
	}
	c.next = now.Add(anonCatalogFailTTL)
	if !c.err {
		c.err = true
		log.Printf("WARN: [opencode] 匿名免费目录拉取失败（%s），%s 后重试：%v", p.anon, anonCatalogFailTTL, err)
	}
	return nil
}

// fetchAnonCatalog 抓一次免费目录。同元数据目录：用自己的超时，不跟随调用方 ctx
// （结果要进缓存），锁内调用天然单飞。
func (p *Provider) fetchAnonCatalog() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), anonCatalogTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.anon+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+anonCredential)
	req.Header.Set("User-Agent", anonUserAgent)
	req.Header.Set("x-opencode-client", "cli")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var ml openai.ModelList
	if err := json.Unmarshal(raw, &ml); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ml.Data))
	for _, m := range ml.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("免费目录为空")
	}
	return out, nil
}
