package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultCookieDomain 是 Google 系 Cookie 的规范域。
const DefaultCookieDomain = ".google.com"

// Cookie 是一个带属性的 Cookie 条目。字段与 Python 参考实现一致。
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"http_only"`
}

// Jar 是一个按名字索引的 Cookie 容器，带域名隔离、过期清理与删除语义。
//
// 与 Python 参考实现的差异：这里额外支持「配置/状态里传入的 Cookie 字符串」
// 作为权威来源（raw 优先），磁盘文件只用于补齐缺失项。原因是本网关的状态层
// （state.Cookies）已经是旋转后 Cookie 的权威持久化点，再让磁盘文件反向覆盖
// 会引入「重启回退到旧 PSIDTS」的风险。逐 Cookie 的域/过期元数据仍会落盘。
type Jar struct {
	mu      sync.Mutex
	cookies map[string]Cookie
	path    string // "" = 纯内存
}

// Parse 从一条 Cookie 请求头字符串构建内存 Jar（不落盘）。
func Parse(raw string) *Jar {
	j := &Jar{cookies: map[string]Cookie{}}
	j.MergeFrom(raw)
	return j
}

// NewJar 创建（可选持久化的）Jar。accountKey 用于派生文件名；dir 为空表示
// 纯内存。磁盘上已有的 Cookie 会先加载，随后由 MergeFrom 补齐配置里缺失的项。
func NewJar(accountKey, dir string) *Jar {
	j := &Jar{cookies: map[string]Cookie{}}
	if strings.TrimSpace(dir) != "" {
		j.path = filepath.Join(dir, cookieFileName(accountKey))
		j.load()
	}
	return j
}

// AccountKey 从一条 Cookie 字符串里取持久化键：优先 __Secure-1PSID（Google 的
// 稳定身份 Cookie，PSIDTS 会轮换而 PSID 不会），取不到则退化为整串的哈希。
// 用 PSID 作键可以让「重新粘贴一份新 Cookie」自然落到新的文件，避免被旧文件
// 里的过期 Cookie 遮蔽。
func AccountKey(raw string) string {
	if v := Parse(raw).Get("__Secure-1PSID"); v != "" {
		return v
	}
	return raw
}

// Path 返回持久化路径（空 = 纯内存）。
func (j *Jar) Path() string { return j.path }

// Get 返回单个 Cookie 值。
func (j *Jar) Get(name string) string {
	if j == nil {
		return ""
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cookies[name].Value
}

// Set 手动写入单个 Cookie（空值视为删除）。
func (j *Jar) Set(name, value string) {
	if j == nil || name == "" {
		return
	}
	j.mu.Lock()
	if value == "" {
		delete(j.cookies, name)
	} else {
		j.cookies[name] = Cookie{
			Name:   name,
			Value:  value,
			Domain: DefaultCookieDomain,
			Path:   "/",
			Secure: strings.HasPrefix(name, "__Secure"),
		}
	}
	j.mu.Unlock()
	j.Persist()
}

// Remove 删除单个 Cookie。
func (j *Jar) Remove(name string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	delete(j.cookies, name)
	j.mu.Unlock()
	j.Persist()
}

// Names 返回排序后的 Cookie 名列表（会先清理过期项）。
func (j *Jar) Names() []string {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cleanupLocked()
	out := make([]string, 0, len(j.cookies))
	for n := range j.cookies {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// All 返回指定域下所有有效 Cookie。
func (j *Jar) All(domains ...string) map[string]string {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cleanupLocked()
	out := map[string]string{}
	for name, c := range j.cookies {
		if domainMatch(c.Domain, domains) {
			out[name] = c.Value
		}
	}
	return out
}

// MergeFrom 把一条 Cookie 请求头字符串合并进 Jar：**传入值优先**，同名会覆盖
// Jar 里已有的值；不在传入串里的既有项（通常来自磁盘缓存）保持原样。
//
// 于是「先加载磁盘、再 MergeFrom(配置/状态)」的顺序下，权威来源是调用方传入的
// Cookie 串（state.Cookies 里旋转后的最新值），磁盘文件只用来补齐缺失项——避免
// 重启后磁盘里的旧 PSIDTS 反向覆盖内存里的新值（与参考实现的「磁盘优先」相反，
// 见 Jar 注释）。
func (j *Jar) MergeFrom(raw string) {
	if j == nil {
		return
	}
	parsed := splitCookieHeader(raw)
	j.mu.Lock()
	defer j.mu.Unlock()
	for k, v := range parsed {
		if ex, ok := j.cookies[k]; ok && ex.Value == v {
			continue
		}
		j.cookies[k] = Cookie{
			Name:   k,
			Value:  v,
			Domain: DefaultCookieDomain,
			Path:   "/",
			Secure: strings.HasPrefix(k, "__Secure"),
		}
	}
}

// CookieHeader 渲染为 Cookie 请求头值（按名排序，稳定可比较）。
// domains 为空表示不过滤；给定域名时只输出匹配的 Cookie（域名隔离）。
func (j *Jar) CookieHeader(domains ...string) string {
	if j == nil {
		return ""
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cleanupLocked()
	names := make([]string, 0, len(j.cookies))
	for name, c := range j.cookies {
		if domainMatch(c.Domain, domains) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+j.cookies[n].Value)
	}
	return strings.Join(parts, "; ")
}

// UpdateFromResponse 从响应里提取 Set-Cookie 并合并进 Jar。
//
// 同名跨域（Google 会把用户跳到 .google.com.hk 等国家域名）按「规范域
// （*.google.com）优先，同级后者覆盖」取舍，取舍只由域名决定、不依赖遍历顺序。
//
// 纵深防御：整段包 recover，Cookie 层任何异常都不该打断调用方流程。
func (j *Jar) UpdateFromResponse(resp *http.Response) {
	if j == nil || resp == nil {
		return
	}
	defer func() { _ = recover() }()

	type candidate struct {
		rank     int
		cookie   Cookie
		deletion bool
	}
	best := map[string]candidate{}
	consider := func(c Cookie, deletion bool) {
		rank := 0
		if isCanonicalDomain(c.Domain) {
			rank = 1
		}
		if prev, ok := best[c.Name]; ok && prev.rank > rank {
			return // 规范域已在手，非规范域无法覆盖
		}
		best[c.Name] = candidate{rank: rank, cookie: c, deletion: deletion}
	}

	now := float64(time.Now().Unix())
	for _, raw := range resp.Header.Values("Set-Cookie") {
		name, value, domain, expires, ok := parseSetCookie(raw)
		if !ok {
			continue
		}
		deletion := value == "" || (expires != nil && *expires <= now)
		exp := 0.0
		if expires != nil {
			exp = *expires
		}
		consider(Cookie{
			Name:    name,
			Value:   value,
			Domain:  domain,
			Path:    "/",
			Expires: exp,
			Secure:  strings.HasPrefix(name, "__Secure"),
		}, deletion)
	}

	changed := false
	for name, c := range best {
		if c.deletion {
			if j.delete(name) {
				changed = true
			}
			continue
		}
		if j.upsert(c.cookie) {
			changed = true
		}
	}
	if changed {
		j.Persist()
	}
}

// Persist 原子落盘（纯内存 Jar 为 no-op）。
func (j *Jar) Persist() {
	if j == nil || j.path == "" {
		return
	}
	j.mu.Lock()
	snapshot := make([]Cookie, 0, len(j.cookies))
	for _, c := range j.cookies {
		snapshot = append(snapshot, c)
	}
	path := j.path
	j.mu.Unlock()

	sort.Slice(snapshot, func(a, b int) bool { return snapshot[a].Name < snapshot[b].Name })
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(path, raw, 0o600)
}

// ---------------------------------------------------------------------------
// 内部

func (j *Jar) upsert(c Cookie) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	ex, ok := j.cookies[c.Name]
	if ok && ex.Value == c.Value && ex.Expires == c.Expires {
		return false
	}
	j.cookies[c.Name] = c
	return true
}

func (j *Jar) delete(name string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.cookies[name]; !ok {
		return false
	}
	delete(j.cookies, name)
	return true
}

// cleanupLocked 删除已过期 Cookie。调用方需持有 j.mu。这里不落盘：被删的项在
// 下次 Persist 时自然消失，避免每次请求都触发一次 IO。
func (j *Jar) cleanupLocked() {
	now := float64(time.Now().Unix())
	for name, c := range j.cookies {
		if c.Expires > 0 && c.Expires < now {
			delete(j.cookies, name)
		}
	}
}

func (j *Jar) load() {
	raw, err := os.ReadFile(j.path)
	if err != nil {
		return
	}
	var items []Cookie
	if err := json.Unmarshal(raw, &items); err != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, item := range items {
		if item.Name == "" {
			continue // 坏记录单条跳过，保留其余有效 Cookie
		}
		if item.Domain == "" {
			item.Domain = DefaultCookieDomain
		}
		if item.Path == "" {
			item.Path = "/"
		}
		j.cookies[item.Name] = item
	}
}

// cookieFileName 用 sha256(accountKey) 的前 16 个十六进制字符作为文件名。
func cookieFileName(accountKey string) string {
	sum := sha256.Sum256([]byte(accountKey))
	return hex.EncodeToString(sum[:])[:16] + ".json"
}

// splitCookieHeader 解析 "a=1; b=2" 形式，忽略空段与无值段；同名后者覆盖前者。
func splitCookieHeader(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

// domainMatch 复刻参考实现的域匹配：domain 为空列表 = 全放行；否则 Cookie 的
// 域需包含过滤串或以之结尾。
func domainMatch(cookieDomain string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	cd := strings.ToLower(cookieDomain)
	for _, f := range filters {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		if strings.Contains(cd, f) || strings.HasSuffix(cd, f) {
			return true
		}
	}
	return false
}

// isCanonicalDomain 报告域是否属于 google.com（用于同名冲突取舍）。
func isCanonicalDomain(domain string) bool {
	d := strings.ToLower(strings.Trim(strings.TrimSpace(domain), "."))
	return d == "google.com" || strings.HasSuffix(d, ".google.com")
}

// parseSetCookie 解析单条 Set-Cookie，返回 (name, value, domain, expires, ok)。
// expires 为绝对 Unix 秒；无 Max-Age/Expires 时为 nil。Max-Age 优先级高于
// Expires（RFC 6265）；Max-Age<=0 或 Expires 在过去会得到一个过去时间戳，
// 供上层识别为删除指令。domain 缺省为 DefaultCookieDomain。
func parseSetCookie(raw string) (string, string, string, *float64, bool) {
	if raw == "" {
		return "", "", "", nil, false
	}
	parts := strings.Split(raw, ";")
	pair := strings.TrimSpace(parts[0])
	i := strings.Index(pair, "=")
	if i < 0 {
		return "", "", "", nil, false
	}
	name := strings.TrimSpace(pair[:i])
	value := strings.TrimSpace(pair[i+1:])
	if name == "" {
		return "", "", "", nil, false
	}

	domain := DefaultCookieDomain
	var (
		maxAge      string
		expiresAttr string
		hasMaxAge   bool
	)
	for _, attr := range parts[1:] {
		j := strings.Index(attr, "=")
		if j < 0 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(attr[:j]))
		av := strings.TrimSpace(attr[j+1:])
		switch k {
		case "max-age":
			maxAge = av
			hasMaxAge = true
		case "expires":
			expiresAttr = av
		case "domain":
			if av != "" {
				domain = av
			}
		}
	}

	var expires *float64
	switch {
	case hasMaxAge:
		if n, err := strconv.Atoi(maxAge); err == nil {
			v := float64(time.Now().Unix() + int64(n))
			expires = &v
		}
	case expiresAttr != "":
		if t, err := http.ParseTime(expiresAttr); err == nil {
			v := float64(t.Unix())
			expires = &v
		}
	}
	return name, value, domain, expires, true
}
