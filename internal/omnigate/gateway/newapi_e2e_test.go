package gateway

// 用本地假 AnyRouter 服务端把「登录抓取 → 查额度 → 每日签到 → 聊天」整条链路
// 真实跑通。这是唯一没法只靠单元测试证明的部分：新增的 cookie 鉴权、New-Api-User
// 身份头、Anthropic 方言和 Claude Code 身份必须**同时**在一个真实 HTTP 往返里
// 成立，否则就会出现「每个部件单独都对、合起来上游仍然拒绝」的情况。
//
// 假上游刻意模仿 AnyRouter / AgentRouter 的三条硬性约束：
//   - 用户接口用浏览器会话 Cookie 鉴权，带 Bearer 不算数；签到这类「写操作」
//     还必须带 New-Api-User: <数字 ID>，否则 401（真实站点正是这样防重放）
//   - 签到路径是 /api/user/sign_in（不是默认的 /api/user/checkin）
//   - 聊天只收 /v1/messages + x-api-key，且要求客户端形态像 Claude Code，
//     收到 Authorization: Bearer 直接 401（真实站点正是这样区分官方 CLI）
//
// /api/user/self 默认只要求 Cookie：这是「抓取包没带用户 ID 时网关能自己去查一次」
// 的前提。更严格的一类站点连 self 都要 New-Api-User，由 selfNeedsUserHeader 模拟，
// 那条路必须靠抓取脚本回传 user_id（真实 cookie 脚本就是这么做的）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
)

// anyRouterFake 是一个最小的 AnyRouter/AgentRouter 仿真。
type anyRouterFake struct {
	mu sync.Mutex

	selfCalls    int
	checkinCalls int
	chatCalls    int

	// 记录聊天请求的实际形态，供断言使用。
	chatUA        string
	chatAPIKey    string
	chatAuthz     string
	chatVersion   string
	chatPath      string
	chatBodyModel string
	chatHasSystem bool

	modelsCalls int
	modelsAuth  string

	// 让签到可以切换成「今日已签到」，验证中性结果。
	alreadyCheckedIn bool

	// selfNeedsUserHeader 模拟更严格的一类站点：连 /api/user/self 都要求
	// New-Api-User。此时唯一可行的路径就是抓取包里自带 user_id。
	selfNeedsUserHeader bool
}

func (f *anyRouterFake) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/user/self", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.selfCalls++
		needUser := f.selfNeedsUserHeader
		f.mu.Unlock()
		// self 默认只校验会话 Cookie。New-Api-User 是签到/兑换等写操作才要——
		// 这正是「抓取包没带用户 ID 时网关能自己去查一次」的前提。
		if needUser {
			if !f.userOK(w, r) {
				return
			}
		} else if !f.cookieOK(w, r) {
			return
		}
		writeFakeJSON(w, `{"success":true,"message":"","data":{
			"id":42,"username":"anyuser","display_name":"Any User","quota":500000,
			"group":"default"}}`)
	})

	mux.HandleFunc("/api/user/sign_in", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.checkinCalls++
		already := f.alreadyCheckedIn
		f.mu.Unlock()
		if !f.userOK(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if already {
			writeFakeJSON(w, `{"success":false,"message":"今日已签到"}`)
			return
		}
		writeFakeJSON(w, `{"success":true,"message":"签到成功，获得 100 额度"}`)
	})

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.modelsCalls++
		f.modelsAuth = r.Header.Get("Authorization")
		f.mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer sk-") {
			writeFakeJSON(w, `{"error":{"message":"invalid token"}}`)
			return
		}
		writeFakeJSON(w, `{"object":"list","data":[
			{"id":"claude-sonnet-4","object":"model","owned_by":"anthropic"},
			{"id":"claude-opus-4","object":"model","owned_by":"anthropic"}]}`)
	})

	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.chatCalls++
		f.chatUA = r.Header.Get("User-Agent")
		f.chatAPIKey = r.Header.Get("x-api-key")
		f.chatAuthz = r.Header.Get("Authorization")
		f.chatVersion = r.Header.Get("anthropic-version")
		f.chatPath = r.URL.Path
		var body struct {
			Model  string `json:"model"`
			System string `json:"system"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.chatBodyModel = body.Model
		f.chatHasSystem = body.System != ""
		f.mu.Unlock()

		// 真实站点用这些条件区分「官方 Claude Code」与「随便一个 HTTP 客户端」。
		if r.Header.Get("Authorization") != "" {
			writeFakeJSON(w, `{"error":{"type":"authentication_error","message":"bearer not accepted; use x-api-key"}}`)
			return
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "claude-cli/") {
			writeFakeJSON(w, `{"error":{"type":"invalid_request_error","message":"only Claude Code traffic is allowed"}}`)
			return
		}
		if r.Header.Get("x-api-key") == "" {
			writeFakeJSON(w, `{"error":{"type":"authentication_error","message":"missing x-api-key"}}`)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frames := []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		}
		for _, fr := range frames {
			_, _ = w.Write([]byte("data: " + fr + "\n\n"))
		}
	})

	return mux
}

// cookieOK 只校验浏览器会话 Cookie。
func (f *anyRouterFake) cookieOK(w http.ResponseWriter, r *http.Request) bool {
	if !strings.Contains(r.Header.Get("Cookie"), "session=") {
		writeFakeJSON(w, `{"success":false,"message":"未登录"}`)
		return false
	}
	return true
}

// userOK 复现 Cookie 型用户接口的鉴权：没有 Bearer 可用，只要 Cookie 会话与
// 数字用户 ID 头。任一缺失就按真实的 401 返回。
func (f *anyRouterFake) userOK(w http.ResponseWriter, r *http.Request) bool {
	if !f.cookieOK(w, r) {
		return false
	}
	if r.Header.Get("New-Api-User") == "" {
		writeFakeJSON(w, `{"success":false,"message":"缺少 New-Api-User"}`)
		return false
	}
	return true
}

func writeFakeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// anyRouterGateway 用真实 New() 构造网关（不是字面量），确保 specOf → 插件
// Factory → 适配器这条生产路径全部走到。
func anyRouterGateway(t *testing.T, srv *httptest.Server, typ string) *Gateway {
	t.Helper()
	cfg := &config.Config{
		DataDir:         t.TempDir(),
		DefaultProvider: "anyrouter",
		DefaultModel:    "claude-sonnet-4",
		Providers: []config.ProviderConfig{{
			Name:         "anyrouter",
			Type:         typ,
			BaseURL:      srv.URL,
			Protocol:     "anthropic",
			ClientPreset: "claude-code",
			AuthMode:     "cookie",
			UserHeader:   "New-Api-User",
			CheckinPath:  "/api/user/sign_in",
			SigninPath:   "/login",
		}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("配置应合法: %v", err)
	}
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg, st, logx.New(10))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

// TestAnyRouterEndToEnd 是这次改动的核心验收：一个只收 Claude Code 流量、
// 用 Cookie 鉴权、签到走 sign_in 的中转站，从「浏览器登录抓取」到「聊天成功」
// 全程不需要任何特判代码。
func TestAnyRouterEndToEnd(t *testing.T) {
	fake := &anyRouterFake{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	g := anyRouterGateway(t, srv, "newapi")

	// 1) 发起登录：必须告诉面板这是 cookie 型站点，并给出站点自己的登录页与
	//    self 路径（面板据此选另一套抓取脚本）。
	auth, err := g.NewAPIAuthorize("anyrouter", "", "")
	if err != nil {
		t.Fatalf("NewAPIAuthorize: %v", err)
	}
	if auth["auth_mode"] != "cookie" {
		t.Errorf("auth_mode = %q, want cookie", auth["auth_mode"])
	}
	if !strings.HasSuffix(auth["signin_url"], "/login") {
		t.Errorf("signin_url = %q, 应为站点自己的登录页", auth["signin_url"])
	}
	if auth["self_path"] != "/api/user/self" {
		t.Errorf("self_path = %q", auth["self_path"])
	}

	// 2) 控制台脚本回传：只有 Cookie，没有 access_token。昵称与用户 ID 留空，
	//    迫使网关自己去 /api/user/self 补全 —— 这正是真实 cookie 脚本的行为。
	res, err := g.NewAPICapture(context.Background(), auth["state"], NewAPICapturePayload{
		SessionCookie: "session=abc123",
		APIKey:        "sk-chat",
	})
	if err != nil {
		t.Fatalf("NewAPICapture: %v", err)
	}
	if res.Label == "" {
		t.Fatal("账号应被建出来（自动命名）")
	}
	if res.UserID != "42" {
		t.Errorf("user_id = %q, 应从 /api/user/self 补全为 42", res.UserID)
	}
	if !res.HasAPIKey {
		t.Error("抓取包里带了 sk- 密钥，应记录为已配置")
	}
	if fake.selfCalls == 0 {
		t.Error("昵称/ID 缺失时应真的去查一次 /api/user/self")
	}

	// 3) 账号列表：Cookie 型账号必须被认作「有凭据」，不能被标成未配置。
	list, err := g.NewAPIAccounts(context.Background(), "anyrouter", true)
	if err != nil {
		t.Fatalf("NewAPIAccounts: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 个账号，实际 %d", len(list))
	}
	if list[0]["has_token"] != true {
		t.Error("Cookie 型账号的 has_token 应为 true，否则面板会显示「未配置凭证」")
	}
	if list[0]["auth_invalid"] == true {
		t.Errorf("账号可用，不该判为授权失效: %+v", list[0])
	}
	if _, ok := list[0]["balance_error"]; ok {
		t.Errorf("查额度应成功，不该有 balance_error: %+v", list[0])
	}

	// 4) 每日签到：走的是 sign_in，不是默认 checkin。
	out, err := g.NewAPICheckin(context.Background(), "anyrouter", "")
	if err != nil {
		t.Fatalf("NewAPICheckin: %v", err)
	}
	if len(out) != 1 || out[0].Error != "" {
		t.Fatalf("签到应成功: %+v", out)
	}
	if fake.checkinCalls != 1 {
		t.Errorf("签到应只发 1 次请求，实际 %d", fake.checkinCalls)
	}

	// 5) 聊天：方言、鉴权、身份三者必须同时正确。
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), &openai.ChatRequest{
		Model: "anyrouter/claude-sonnet-4",
		Messages: []openai.Message{
			{Role: "system", Content: openai.Content{Text: "be brief"}},
			{Role: "user", Content: openai.Content{Text: "hi"}},
		},
	}, w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("客户端应收到解码后的正文，得到 %s", w.Body.String())
	}
	if fake.chatPath != "/v1/messages" {
		t.Errorf("聊天端点 = %q, want /v1/messages", fake.chatPath)
	}
	if fake.chatAPIKey != "sk-chat" {
		t.Errorf("x-api-key = %q", fake.chatAPIKey)
	}
	if fake.chatAuthz != "" {
		t.Errorf("Anthropic 方言不该带 Authorization，得到 %q", fake.chatAuthz)
	}
	if !strings.HasPrefix(fake.chatUA, "claude-cli/") {
		t.Errorf("User-Agent = %q，身份没有生效", fake.chatUA)
	}
	if fake.chatVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q", fake.chatVersion)
	}
	if fake.chatBodyModel != "claude-sonnet-4" {
		t.Errorf("上游模型 = %q，provider 前缀应被剥掉", fake.chatBodyModel)
	}
	if !fake.chatHasSystem {
		t.Error("system 消息应被翻译成顶层 system 字段")
	}
}

// TestAnyRouterCheckinAlreadyClaimedIsNeutral 钉住「今日已签到」不算失败：
// 这是最常见的结果（用户可能手动签过），报错会让调度器日志天天飘红。
func TestAnyRouterCheckinAlreadyClaimedIsNeutral(t *testing.T) {
	fake := &anyRouterFake{alreadyCheckedIn: true}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	g := anyRouterGateway(t, srv, "newapi")
	auth, err := g.NewAPIAuthorize("anyrouter", "acc", "sk-chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.NewAPICapture(context.Background(), auth["state"], NewAPICapturePayload{
		SessionCookie: "session=abc123",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := g.NewAPICheckin(context.Background(), "anyrouter", "acc")
	if err != nil {
		t.Fatalf("NewAPICheckin: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("应有 1 条结果，实际 %d", len(out))
	}
	if out[0].Error != "" {
		t.Errorf("「今日已签到」应视为成功（中性），得到错误 %q", out[0].Error)
	}
}

// TestAnyRouterStrictSelfUsesCapturedUserID 覆盖更严格的一类站点：连
// /api/user/self 都要求 New-Api-User。这时网关不能靠「自己查一次」补全身份，
// 必须用抓取包带回来的 user_id —— 真实抓取脚本正是先去 self 拿到 id 再回传，
// 所以这条路径才是线上主路径，不能只有宽松版被验证过。
func TestAnyRouterStrictSelfUsesCapturedUserID(t *testing.T) {
	fake := &anyRouterFake{selfNeedsUserHeader: true}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	g := anyRouterGateway(t, srv, "newapi")
	auth, err := g.NewAPIAuthorize("anyrouter", "", "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.NewAPICapture(context.Background(), auth["state"], NewAPICapturePayload{
		SessionCookie: "session=abc123",
		UserID:        "42",
		Username:      "anyuser",
		APIKey:        "sk-chat",
	})
	if err != nil {
		t.Fatalf("NewAPICapture: %v", err)
	}
	if res.UserID != "42" {
		t.Errorf("user_id = %q，应保留抓取包带回的 42", res.UserID)
	}
	// 抓取包里带了昵称，就不该再发一次 self 探测。
	if fake.selfCalls != 0 {
		t.Errorf("昵称与 ID 都齐时不该再查 self，实际 %d 次", fake.selfCalls)
	}

	out, err := g.NewAPICheckin(context.Background(), "anyrouter", "")
	if err != nil {
		t.Fatalf("NewAPICheckin: %v", err)
	}
	if len(out) != 1 || out[0].Error != "" {
		t.Fatalf("签到应成功: %+v", out)
	}

	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), &openai.ChatRequest{
		Model:    "anyrouter/claude-sonnet-4",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}, w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}

// TestAnyRouterMissingUserIDIsNotFabricated 钉住一个真实缺陷：身份补全失败时
// **不能**拿随机 UUID 冒充用户 ID。Cookie 型站点把它当 New-Api-User 发出去，
// 假 ID 换来的是必然的 401，而且会随账号落库、之后每次刷新都在延续这个错误。
// 正确做法是留空，让用户接口明确报「缺少 New-Api-User」，用户补一次抓取即可。
func TestAnyRouterMissingUserIDIsNotFabricated(t *testing.T) {
	// self 也要求 New-Api-User，而抓取包里恰好没带 ID —— 补全必然失败。
	fake := &anyRouterFake{selfNeedsUserHeader: true}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	g := anyRouterGateway(t, srv, "newapi")
	auth, err := g.NewAPIAuthorize("anyrouter", "acc", "sk-chat")
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.NewAPICapture(context.Background(), auth["state"], NewAPICapturePayload{
		SessionCookie: "session=abc123",
	})
	if err != nil {
		t.Fatalf("补全失败不该让抓取整体失败: %v", err)
	}
	if res.UserID != "" {
		t.Fatalf("user_id = %q，宁可留空也不该编造（编造的 ID 会让签到永远 401）", res.UserID)
	}

	// 账号必须仍然存在、且被认作有凭据（问题出在身份头，不是会话）。
	list, err := g.NewAPIAccounts(context.Background(), "anyrouter", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0]["has_token"] != true {
		t.Fatalf("账号应存在且有会话凭据: %+v", list)
	}
	// 但没有 ID 时，本地就能判定它不可用，而且提示要能指路——不能只显示「失败」。
	if msg, _ := list[0]["auth_error"].(string); !strings.Contains(msg, "New-Api-User") {
		t.Errorf("auth_error 应点明缺 New-Api-User，得到 %q", msg)
	}

	// 补一次抓取（这次带上 ID）后，同一账号应立刻恢复可用 —— 验证留空是可修复的，
	// 而不是把账号毁成永久坏状态。同一个浏览器会话不能被当成第二个账号。
	auth2, err := g.NewAPIAuthorize("anyrouter", "acc", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.NewAPICapture(context.Background(), auth2["state"], NewAPICapturePayload{
		SessionCookie: "session=abc123",
		UserID:        "42",
		Username:      "anyuser",
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(g.Accounts("anyrouter")); n != 1 {
		t.Fatalf("同一会话补抓不该新增账号，实际有 %d 条", n)
	}
	out, err := g.NewAPICheckin(context.Background(), "anyrouter", "acc")
	if err != nil {
		t.Fatalf("NewAPICheckin: %v", err)
	}
	if len(out) != 1 || out[0].Error != "" {
		t.Fatalf("补抓后签到应成功: %+v", out)
	}
}

// TestAnyRouterModelCatalogUsesChatKey 钉住两条一起才能生效的事：
//
//  1. pickAccount 不能只看 Cookie —— 账号很可能只有 sk- 密钥（模型目录就是用它拉的）
//  2. 目录走真实上游（/v1/models）而不是内置兜底清单
//
// 只看 Cookie 时 pickAccount 返回 nil，ListModelsWithAccount 会静默退回内置清单：
// 面板上「刷新目录」看似成功，实际拿到的是写死的几个模型名——这是最难发现的
// 一类故障（不报错、但永远看不到站点真实模型）。
func TestAnyRouterModelCatalogUsesChatKey(t *testing.T) {
	fake := &anyRouterFake{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	g := anyRouterGateway(t, srv, "newapi")
	auth, err := g.NewAPIAuthorize("anyrouter", "acc", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.NewAPICapture(context.Background(), auth["state"], NewAPICapturePayload{
		SessionCookie: "session=abc123",
		UserID:        "42",
		Username:      "anyuser",
		APIKey:        "sk-chat",
	}); err != nil {
		t.Fatal(err)
	}

	models, err := g.fetchModels(context.Background(), "anyrouter")
	if err != nil {
		t.Fatalf("拉取目录: %v", err)
	}
	if fake.modelsCalls != 1 {
		t.Fatalf("/v1/models 应被调用 1 次，实际 %d 次（0 次 = 退回内置清单）", fake.modelsCalls)
	}
	if fake.modelsAuth != "Bearer sk-chat" {
		t.Errorf("目录请求应带账号的 sk- 密钥，得到 %q", fake.modelsAuth)
	}
	if len(models) != 2 || models[0].ID != "claude-sonnet-4" {
		t.Fatalf("目录应来自上游真实清单，得到 %+v", models)
	}
}

// TestNewAPIWithoutCookieAuthKeepsBearerBehaviour 反向保护：一个**没有**配置
// cookie 鉴权的 newapi 站点必须仍然走原来的 Bearer 路径。这条路径支撑着线上
// 现有的 justwoker 类账号，不能被新逻辑顺手改掉。
func TestNewAPIWithoutCookieAuthKeepsBearerBehaviour(t *testing.T) {
	var gotAuth, gotUserHeader, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUserHeader = r.Header.Get("New-Api-User")
		gotPath = r.URL.Path
		writeFakeJSON(w, `{"success":true,"data":{"id":7,"username":"legacy"}}`)
	}))
	defer srv.Close()

	cfg := &config.Config{
		DataDir: t.TempDir(),
		Providers: []config.ProviderConfig{{
			Name: "legacy", Type: "newapi", BaseURL: srv.URL,
		}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg, st, logx.New(10))
	if err != nil {
		t.Fatal(err)
	}

	auth, err := g.NewAPIAuthorize("legacy", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if auth["auth_mode"] != "bearer" {
		t.Errorf("auth_mode = %q, want bearer（老站点行为不能变）", auth["auth_mode"])
	}
	if !strings.HasSuffix(auth["signin_url"], "/sign-in") {
		t.Errorf("signin_url = %q, 老站点默认登录页是 /sign-in", auth["signin_url"])
	}

	// 老的 access_token 抓取路径必须仍然工作。
	if _, err := g.NewAPICapture(context.Background(), auth["state"], NewAPICapturePayload{
		AccessToken: "Bearer at-123",
		Username:    "legacy",
		UserID:      "7",
	}); err != nil {
		t.Fatalf("bearer 型站点应仍能抓取: %v", err)
	}

	list, err := g.NewAPIAccounts(context.Background(), "legacy", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0]["has_token"] != true {
		t.Fatalf("bearer 账号应被认作有凭据: %+v", list)
	}
	if gotPath != "/api/user/self" {
		t.Errorf("查额度路径 = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("bearer 型站点仍应发 Authorization，得到 %q", gotAuth)
	}
	if gotUserHeader != "" {
		t.Errorf("没配 user_header 时不该凭空加 New-Api-User，得到 %q", gotUserHeader)
	}
}
