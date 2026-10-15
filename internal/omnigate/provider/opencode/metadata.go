package opencode

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// DefaultMetaURL 是上游公开的机器可读模型目录（models.dev 形态）。
//
// 为什么要多拉这一份：/v1/models 每条只有 id/object/created/owned_by 四个字段
// （2026-10-15 实测 86+37 条全部如此），上下文长度、输出上限、价格、免费标记一概没有
// ——面板的「价格」「上下文」「上限」三列对 opencode 全是「—」。而这份目录里每个模型
// 都带 limit.context / limit.output / cost。
//
// 它填的还是同一份值的**权威来源**：internal/upstream 的 context_length /
// max_output_tokens 查找链以「上游动态值优先」（见 context_catalog.go 文件头），填上
// 它，客户端拿到的上下文长度就不必退化到静态知识表与 1M 兜底。
const DefaultMetaURL = "https://models.opencode.ai/api.json"

// 元数据抓取的节奏与上限。目录当前 5.6MB（2026-10-15 实测 200 OK / 5 666 819 字节，
// 230 个 provider），抓一次一两秒，所以按小时级缓存；失败短退避，避免每次拉模型目录
// 都白等一遍。
const (
	metaTTL     = 6 * time.Hour
	metaFailTTL = 5 * time.Minute
	metaTimeout = 30 * time.Second
	metaMaxBody = 32 << 20
)

// metaInfo 是一个模型在上游元数据目录里的可用字段（已换算成本项目的单位）。
type metaInfo struct {
	context   int64
	maxTokens int64
	// pricing 是**每 token** 美元价，与 openai.Model.Pricing 同口径：面板显示时乘回
	// 1e6 得到「每百万 token」。
	pricing *openai.ModelPricing
	free    bool
}

// metaIndex 是抓取结果按 tier 建好的索引。
type metaIndex struct {
	byTier map[string]map[string]metaInfo // tier → 模型 id → 元数据
	merged map[string]metaInfo            // 不分 tier（按 pool 顺序先到先得）
}

// metaCache 缓存元数据抓取结果。零值可用：首次调用即抓取。url 为空 = 不抓取。
type metaCache struct {
	url string

	mu   sync.Mutex
	next time.Time
	idx  *metaIndex
	err  bool // 上次抓取是否失败：只用于「状态变化时才打日志」
}

// metaProviderJSON / metaModelJSON 是上游目录里我们用到的那一小部分。整份目录有 230 个
// provider、上万个模型条目，只解出这两个 tier，其余原样跳过。
type metaProviderJSON struct {
	API    string                   `json:"api"`
	Models map[string]metaModelJSON `json:"models"`
}

type metaModelJSON struct {
	Limit *struct {
		Context int64 `json:"context"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	Cost *struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	} `json:"cost"`
}

// metaTierFor 判断目录里的一个 provider 属于哪个 tier。
//
// 实测目录里的两个 id 是 "opencode"（zen）与 "opencode-go"（go），api 字段分别是
// https://opencode.ai/inference/v1 与 https://opencode.ai/inference/go/v1。**先判 go**：
// "opencode-go" 里也含 "opencode"，顺序反了就会把 go 的元数据算到 zen 头上。api 字段也
// 参与判断，这样万一上游改了 id 还能对上。
func metaTierFor(providerID, api string) string {
	v := strings.ToLower(providerID + " " + api)
	switch {
	case strings.Contains(v, "opencode-go"), strings.Contains(v, "/go/"):
		return TierGo
	case strings.Contains(v, "opencode"), strings.Contains(v, "/zen/"):
		return TierZen
	}
	return ""
}

// toInfo 把上游条目换算成本项目的单位。
//
// 价格单位是这里唯一容易错的地方：目录里 cost 是「每百万 token 美元」，而
// openai.Model.Pricing 约定「每 token 美元」（面板按 v*1e6 显示成 $/1M）。不做这个除法
// 就会把 $1.40/1M 显示成 $1,400,000.00/1M。
func (m metaModelJSON) toInfo() metaInfo {
	var info metaInfo
	if m.Limit != nil {
		info.context = m.Limit.Context
		info.maxTokens = m.Limit.Output
	}
	if m.Cost != nil {
		if m.Cost.Input == 0 && m.Cost.Output == 0 {
			// 免费模型在目录里就是全 0 价：实测 2026-10-15 有 15 个全 0 价模型，
			// 其中 13 个名字带 -free，另有 big-pickle / grok-code 两个不带。
			// 所以按价格判免费，而不是按名字。
			info.free = true
		} else {
			const perMillion = 1e6
			info.pricing = &openai.ModelPricing{
				Input:           m.Cost.Input / perMillion,
				Output:          m.Cost.Output / perMillion,
				InputCacheRead:  m.Cost.CacheRead / perMillion,
				InputCacheWrite: m.Cost.CacheWrite / perMillion,
			}
		}
	}
	return info
}

// metaIndex 返回元数据索引；未到刷新时间就直接给缓存。
//
// 抓取失败**不丢旧数据**，只是短暂退避后重试：元数据只用在锦上添花的位置（列表显示、
// 上下文长度兜底），绝不能让它把模型目录本身拖垮或拖慢。
//
// 锁内抓取：并发调用会排队等同一次抓取，等价于单飞，不会出现同时下几份 5.6MB。
func (p *Provider) metaIndex() *metaIndex {
	if p == nil || p.meta.url == "" {
		return nil
	}
	c := &p.meta
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idx != nil && time.Now().Before(c.next) {
		return c.idx
	}
	idx := p.fetchMeta()
	now := time.Now()
	if idx != nil {
		if c.err {
			log.Printf("INFO: [opencode] 模型元数据已恢复（%s）", c.url)
		}
		c.err = false
		c.idx = idx
		c.next = now.Add(metaTTL)
		return c.idx
	}
	c.next = now.Add(metaFailTTL)
	if !c.err {
		c.err = true
		log.Printf("WARN: [opencode] 模型元数据拉取失败（%s）：模型目录将不带上下文长度与价格，%s 后重试",
			c.url, metaFailTTL)
	}
	return c.idx
}

// fetchMeta 拉一次目录并建成索引。任何一步失败都返回 nil（调用方保持 bare 目录）。
//
// 刻意用 context.Background() + 自己的超时，而不是跟随调用方的 ctx：调用方（网关的模型
// 列表）可能提前取消，但这份结果是要进缓存给后续请求用的，半途而废纯属浪费。
func (p *Provider) fetchMeta() *metaIndex {
	ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.meta.url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")

	// 用 provider 自己的 client：出站代理（SetProxy / SetProxySelector）要同样生效。
	resp, err := p.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, metaMaxBody))
	if err != nil {
		return nil
	}

	// 先解成 RawMessage 再挑出两个 tier：整份目录全量反序列化会白白造出几十 MB 的临时
	// 对象（上万个模型条目），而我们只要其中两份。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	idx := &metaIndex{
		byTier: map[string]map[string]metaInfo{},
		merged: map[string]metaInfo{},
	}
	for pid, rp := range top {
		var head struct {
			API string `json:"api"`
		}
		// api 拉不到就只按 provider id 判断归属。
		_ = json.Unmarshal(rp, &head)
		tier := metaTierFor(pid, head.API)
		if tier == "" {
			continue
		}
		var prov metaProviderJSON
		if err := json.Unmarshal(rp, &prov); err != nil {
			continue
		}
		m := idx.byTier[tier]
		if m == nil {
			m = make(map[string]metaInfo, len(prov.Models))
			idx.byTier[tier] = m
		}
		for id, mm := range prov.Models {
			m[id] = mm.toInfo()
		}
	}
	if len(idx.byTier) == 0 {
		// 目录结构变了或返回了别的东西：当失败处理，别把「全空」成功缓存 6 小时。
		return nil
	}

	// merged 按 pool 顺序合并（与模型并集同序）：同一个 id 两个 tier 都有时以先参与轮询
	// 的那个 tier 为准。没配密钥的 tier 也要并进来——配置兜底清单同样值得带上元数据。
	order := make([]string, 0, len(idx.byTier))
	for _, pl := range p.pools {
		order = append(order, pl.name)
	}
	for _, tier := range []string{TierGo, TierZen} {
		if _, ok := idx.byTier[tier]; !ok {
			continue
		}
		seen := false
		for _, name := range order {
			if name == tier {
				seen = true
				break
			}
		}
		if !seen {
			order = append(order, tier)
		}
	}
	for _, tier := range order {
		for id, info := range idx.byTier[tier] {
			if _, dup := idx.merged[id]; dup {
				continue
			}
			idx.merged[id] = info
		}
	}
	if len(idx.merged) == 0 {
		return nil
	}
	return idx
}

// enrich 用该 tier 的元数据补齐目录里缺的字段。
func (p *Provider) enrich(tier string, models []openai.Model) {
	idx := p.metaIndex()
	if idx == nil {
		return
	}
	byID := idx.byTier[tier]
	if byID == nil {
		byID = idx.merged
	}
	applyMeta(byID, models)
}

// enrichMerged 给「配置兜底清单」补元数据：此时模型没有 tier 归属，任一份都行。
func (p *Provider) enrichMerged(models []openai.Model) {
	if idx := p.metaIndex(); idx != nil {
		applyMeta(idx.merged, models)
	}
}

// applyMeta 只补空、不覆盖：上游 /v1/models 将来真带上这些字段时，以它自己给的为准。
func applyMeta(byID map[string]metaInfo, models []openai.Model) {
	for i := range models {
		info, ok := byID[models[i].ID]
		if !ok {
			continue
		}
		if models[i].ContextWindow == 0 {
			models[i].ContextWindow = info.context
		}
		if models[i].MaxTokens == 0 {
			models[i].MaxTokens = info.maxTokens
		}
		if models[i].Pricing == nil {
			models[i].Pricing = info.pricing
		}
		if info.free {
			models[i].IsFree = true
		}
	}
}
