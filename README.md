# OmniGate Panel

单二进制、多上游、多账号的 OpenAI 兼容网关，内置 Web 管理面板。

当前版本已移除旧的 WorkBuddy/CodeBuddy 反代链路，只保留 **OmniGate 引擎**：
一个进程同时承载供应商、账号池、路由、签到、请求记录与面板。代码与配置均不向后
兼容旧版本，升级前请先备份旧目录。

## 特性

- **OpenAI / Anthropic / Gemini 兼容入口**：`/v1/chat/completions`、
  `/v1/responses`、`/v1/messages`、`/v1beta/models/*` 等。
- **多供应商**：raccoon、Runable、OpenCode、New API（含 AnyRouter / AgentRouter）、
  任意 OpenAI 兼容上游、Gemini Web、TRAE。
- **出站身份与协议方言**：`client_preset` 决定上游看到的客户端（omnigate / 浏览器 /
  Claude Code / Codex，含 TLS ClientHello），`protocol` 决定说的是哪种方言
  （chat/completions / Anthropic messages / Responses），两者独立可组合。
- **账号池**：轮询、冷却、积分快过期优先、密钥池健康与自动禁用、会话/账号级粘性。
- **供应商插件 SPI**：类型元数据、字段 schema、能力与操作统一注册；面板表单由
  后端 schema 动态生成，新增 provider 不需要改前端。
- **Web 面板**：原生 ES Module、零 npm、零构建，`go:embed` 进二进制；七页信息
  架构（总览 / 供应商 / 模型 / 路由 / 用量 / 请求 / 系统）。
- **多租户**：admin / viewer / user 三种角色；user 只能看到并运维归属自己的账号。
- **PostgreSQL 配置中心**：供应商、路由、网关密钥、面板账号、会话、归属、用量、
  请求记录与引擎状态全部落库；`config.json` 只保留数据库 DSN。
- **安全默认**：配置 0600 原子写、面板会话鉴权、CSRF 同源校验、密钥脱敏、
  管理端点拒绝收窄密钥。

## 架构

```
cmd/server                 唯一入口：配置 → 引擎 → 面板 → HTTP
internal/omnigate/
  config/                  引擎配置模型（数据存 PostgreSQL panel_config）
  gateway/                 选号、冷却、密钥池、模型目录、路由
  egress/                  出站客户端身份（UA / 特征头 / TLS ClientHello 预设）
  wire/                    出站协议方言（openai / anthropic / responses 编解码）
  provider/                上游实现 + 插件描述符
  plugin/                  SPI（Plugin / Factory / Field / 能力接口）
  server/                  OpenAI / Anthropic / Gemini 协议入口
  scheduler/               定时签到、保活、余额刷新
  state/                   运行时账号与令牌持久化
internal/panel/            面板后端 + 内嵌 SPA（web/）
internal/{pgstore,kv,reqlog,usage,outbound,panelauth}/  共享基础设施
```

请求路径：

```
/v1/*          OpenAI 兼容 API
/v1/messages   Anthropic Messages
/v1beta/*      Gemini 兼容 API
/images/*      生成的图片
/healthz       健康检查
/panel/        管理面板
```

## 快速开始

```bash
# 1) bootstrap：填好 PostgreSQL DSN
cp config.example.json config.json
#    也可以不建文件，直接设置 OMNIGATE_STORAGE_DSN

# 2) 构建
go build -o bin/omnigate-panel ./cmd/server

# 3) 设置第一个面板管理员（账号写 PostgreSQL panel_users）
./bin/omnigate-panel -config config.json -set-admin-password -admin-user admin

# 4) 启动
./bin/omnigate-panel -config config.json
# 面板： http://127.0.0.1:7863/panel/
```

首次启动会把旧版 `config.json` 的配置段与 `omnigate.json` 一次性导入 PostgreSQL
的 `panel_config` 行，然后把 `config.json` 收缩为 DSN bootstrap、把旧引擎配置备份到
`data/backups/config-migration-*`。之后供应商、账号、路由、签到配置全部直接读写数据库，
保存后进程内热重载，无需重启。

## 面板

- **总览**：引擎状态、账号健康、24h 请求/token/积分、近期请求。
- **供应商**：左侧常显列表 + 右侧详情；详情分「概览 / 账号 / 上游密钥 / 模型 /
  高级」，账号就地增删与签到。
- **模型**：目录、价格、能力与可用供应商。
- **路由**：默认供应商/模型、优先级、模型别名映射、客户端密钥 scope 与解析预览。
- **用量**：token、请求、成本与按供应商/账号分组。
- **请求**：请求归档、详情抽屉、运行日志。列表「模型 / 重定向」列与详情里的
  「路由重定向」区块会并排展示**客户端请求的模型**与**真正发往上游的模型**：
  两者不同即说明别名、目录补全或回退换供应商生效过，不必再靠日志猜测。
- **系统**：客户端密钥、面板账号、出站代理、运行参数与存储。

字段表单由 `GET /panel/api/omni/provider-types` 返回的插件元数据驱动。新增
provider 时只需后端提供 descriptor，前端无需改动。

### 别名为什么看起来没生效

模型别名（`model_mapping`）只在**裸模型名**上起作用。排查时按这个顺序看：

1. **请求记录里的「实际模型」**。若它等于请求名，说明别名没命中；若不同，别名
   已生效，`upstream_model` 就是最终打到上游的 ID。
2. **上游网关是否抢先改写了模型名**。new-api 的渠道（channel）自己也有一层
   `model_mapping`，它会在请求到达本服务之前就把裸名改写成带 provider 前缀的
   ID。带前缀的 ID 走「显式指定上游」分支，**不会**再查别名表——别名链因此整条
   失效，而两种情况的日志看起来一模一样。OmniGate 的 `/v1/models` 已经把别名
   作为独立条目暴露，上游渠道通常不需要再配这一层映射。
3. **回退目标是否在入站密钥的 scope 内**。别名链的后续目标由网关按密钥的
   `providers` 白名单过滤，越界的那条会被静默跳过（日志只剩一行「未授权该供应
   商」）。路由页的解析预览会直接标出「该回退会被静默跳过」。

## 供应商插件

一个 provider 由三部分组成：

1. 实现核心接口 `provider.Provider`（`ListModels` / `StreamChat`）；
2. 按需实现可选能力接口（账号模型、余额、签到、登录、Cookie 轮换、动作等）；
3. 注册一份 `plugin.Plugin` 描述符，声明字段 schema、能力、动作和定时任务。

```go
plugin.MustRegister(plugin.Plugin{
    Type:        "my-upstream",
    DisplayName: "My Upstream",
    AccountMode: plugin.AccountModeNone,
    Traits:      plugin.Traits{WireFormat: plugin.WireOpenAI},
    ConfigFields: []plugin.Field{
        {Key: "base_url", Label: "Base URL", Kind: plugin.KindURL, Required: true},
        {Key: "api_key", Label: "API Key", Kind: plugin.KindSecret, Required: true},
    },
    Capabilities: []plugin.Capability{plugin.CapModels},
    Factory: func(s plugin.ProviderSpec, _ plugin.Deps) (plugin.Provider, error) {
        return myprovider.New(s.Name, s.BaseURL, s.APIKey, s.Models), nil
    },
})
```

内置类型注册在 `internal/omnigate/plugin/catalog`，启动时由 `main` 显式调用
`catalog.Register()`；网关工厂按注册表查找 `Factory`，不再有类型 switch。新增
「只是换 base_url」的上游不需要写插件，直接用 `openai` 类型配置即可。

## 出站身份与协议方言

上游看到的「谁在请求」由两层决定，都是供应商级配置，改完保存即热重载：

- `client_preset` — 出站客户端身份（UA + 一组特征头 + TLS ClientHello）
- `protocol` — 出站协议方言（请求体形状 + 端点 + 鉴权头 + 响应解码）

两层是独立的：身份管「看起来像谁」，协议管「说的是哪种话」。只收官方客户端流量
的中转站（AnyRouter 一类）两个都要配对，缺一个都会被拒。

### client_preset

| 值 | 含义 |
| --- | --- |
| `omnigate`（默认） | 如实申报 `omnigate/<ver>`，不做任何伪装 |
| `browser` | 桌面 Chrome：UA、`Accept-Language`、`Sec-Fetch-*`。New API 类型的历史默认就是这个 |
| `claude-code` | Claude Code CLI：`claude-cli/<ver>`、`x-app: cli`、`anthropic-beta`、`x-stainless-*`，并套用 Chrome 的 TLS ClientHello（真实 CLI 不用 Go 的 TLS 栈，只改头不改握手照样能被认出来） |
| `codex` | OpenAI Codex CLI：`codex_cli_rs/<ver>`、`originator`、`OpenAI-Beta: responses=experimental` |

留空表示「沿用该类型的历史默认」：`openai` 类型如实申报，`newapi` 类型伪装浏览器。
预设写死在 `internal/omnigate/egress`，但三个覆盖字段永远优先：

```jsonc
{
  "name": "anyrouter",
  "type": "newapi",
  "client_preset": "claude-code",
  "client_user_agent": "claude-cli/2.0.20 (external, cli)",  // 预设版本过期时改这里
  "client_headers": { "anthropic-beta": "claude-code-20250219" },
  "client_tls_profile": "chrome120"                          // "none" = 显式关闭
}
```

预设只影响「能问出身份」的那部分：`Accept-Encoding` 之类由 net/http 自己协商的
头不写死，手写的 `Host` / `Content-Length` 会被忽略（net/http 拥有它们）。

### protocol

| 值 | 端点 | 鉴权 | 请求/响应 |
| --- | --- | --- | --- |
| `openai`（默认） | `/v1/chat/completions` | `Authorization: Bearer` | OpenAI chat + SSE |
| `anthropic` | `/v1/messages` | `x-api-key`（**不带** Authorization） | Anthropic messages + SSE |
| `responses` | `/v1/responses` | `Authorization: Bearer` | Responses API + SSE |

方言同时决定响应解码：`anthropic` 的 SSE 会被翻译成统一的 `provider.Event`，
工具调用、`cache_creation_input_tokens` 之类的用量字段都不丢。未知值一律降级为
`openai`，不会让供应商变成不可路由。

`protocol` 只对通用后端（`openai` / `newapi`）生效；有自有协议的上游
（raccoon、geminiweb、trae、runable、opencode）忽略它。

### 接入 AnyRouter / AgentRouter（含自动签到）

两者都是 New API 的部署，所以用 `newapi` 类型即可，不需要单独的插件。与默认
部署的差异只有四处，全部在供应商配置里覆盖：

| 配置项 | 值 | 原因 |
| --- | --- | --- |
| `checkin_path` | `/api/user/sign_in` | 默认部署是 `/api/user/checkin` |
| `auth_mode` | `cookie` | 用户接口认浏览器会话 Cookie，没有可轮换的 access_token |
| `user_header` | `New-Api-User` | 上游靠这个头决定「是哪个账号」 |
| `signin_path` | `/login` | 登录向导打开的页面 |

再加上出站身份的两项，一个可用的配置长这样：

```jsonc
{
  "name": "anyrouter",
  "type": "newapi",
  "base_url": "https://anyrouter.top",
  "protocol": "anthropic",
  "client_preset": "claude-code",
  "auth_mode": "cookie",
  "user_header": "New-Api-User",
  "checkin_path": "/api/user/sign_in",
  "signin_path": "/login"
}
```

添加账号的流程：

1. 供应商页 →「登录 / 添加账号」→ 打开登录页，在站点完成 GitHub / LinuxDO 登录。
2. 把面板生成的控制台脚本粘贴进该站点的开发者工具运行。Cookie 型站点的脚本会
   读 `document.cookie`、查一次 `/api/user/self` 补全数字用户 ID，然后回传。
3. 回到面板补填聊天密钥（`sk-...`，站点「令牌」页创建）。**签到与额度查询不需要
   它，但聊天需要**，所以账号建好后要在账号里补。
4. 若脚本提示读不到 Cookie（会话 Cookie 是 httpOnly），改用手动添加：开发者工具
   Application → Cookies 复制整条，填进 `session_cookie`，并把数字用户 ID 填进
   `uid`。

数字用户 ID 是这类站点唯一不可省的字段：上游用 `New-Api-User: <id>` 决定「这是哪个
账号」，缺了它签到与额度一律 401。所以网关**不会**替你编一个：抓取时如果身份补全
失败，账号仍然建好，只是标成「缺少数字用户 ID（New-Api-User）」，重新抓取一次即可
修复。同一个浏览器会话不会被记成第二个账号——补抓会复用原来那条，不会留下一个坏
账号在旁边。

签到是普通的 New API 签到：勾选「定时签到」后由调度器每天按 `checkin_path` 执行，
不需要额外配置。注意 AnyRouter 官方文档声明不转发非 Claude Code 的 API 流量——
用 `client_preset=claude-code` + `protocol=anthropic` 是在协议层做到「真的像」，
但这属于绕过对方的使用约束，是否接受由账号所有者判断，上游也可能因此封号。

模型目录同样是按账号拉的：用账号里的 `sk-` 密钥请求 `/v1/models`。没有密钥时不会
报错，而是退回供应商配置里手写的 `models` 清单——所以「刷新目录」看起来成功、却只有
几个写死的模型名，通常就是缺聊天密钥。

## 配置

### config.json（仅 bootstrap）

```json
{
  "storage": {
    "driver": "postgres",
    "dsn": "postgres://omnigate:change-me@127.0.0.1:5432/omnigate_panel?sslmode=disable"
  }
}
```

- `storage.driver` 必须是 `postgres`；`storage.dsn` 是连接数据库的唯一 bootstrap
  信息，不能用面板修改（数据库无法自己提供自己的地址）。也可用
  `OMNIGATE_STORAGE_DSN` 环境变量替代文件。
- 其余参数（`listen`、`data_dir`、`api_key`、`outbound`、`panel_auth`、`logging`、
  `server`）在首次迁移后保存在 PostgreSQL `panel_config` 行，面板「系统」页直接读写。
- `data_dir`：请求归档、生成图片等本地运行数据的根目录；请求/用量/会话/账号等
  状态的主存储在 PostgreSQL。
- `panel_auth`：面板只认密码登录；账号哈希保存在 `panel_users` 表，`users` 为空时
  用 `-set-admin-password` 引导。

### PostgreSQL `panel_config`

一个版本化 JSON 文档，包含主配置和 OmniGate 引擎配置（供应商、`api_keys`、
`default_provider/model`、`routing.prefer`、`credit_routing`、`checkin`、
`rate_limit`、`fallback`、`fingerprint` 等）。保存时先写上一版到
`panel_config_previous`，再覆盖当前行；当前行损坏时启动自动回退到上一版。
引擎配置结构参考 `omnigate.example.json`，但运行时不再读取该文件。

## 客户端密钥

`api_key` 是面板自己的全量密钥。面向客户端的密钥放在 PostgreSQL `panel_config`
的引擎配置 `api_keys`：

```json
"api_keys": [
  "sk-full",
  { "key": "sk-raccoon-only", "name": "raccoon 专用", "providers": ["raccoon"] }
]
```

收窄密钥只能调用其授权供应商的模型端点；`/admin/*` 管理端点会返回 403。

## 多租户

- **admin**：全部账号与系统配置。
- **viewer**：只读全部。
- **user**：只能查看/运维归属自己的账号（归属键 `omni:<provider>/<label>`）。

自助注册默认开启（可在面板关闭）；注册限流按来源 IP 固定窗口计数。

## Docker

```bash
cp config.example.json config.json
mkdir -p data
docker compose up -d --build
```

`config.json` 只填 PostgreSQL DSN；首次启动会建表、迁移旧配置并写入
`panel_config`。面板密码用：

```bash
docker compose exec omnigate-panel /app/omnigate-panel \
  -config /app/config.json -set-admin-password -admin-user admin
```

> ⚠️ **反向代理必须放宽超时**，否则会出现「后端已计费、客户端却收到 504 并重试」
> 的 token 浪费。见下文[反向代理部署](#反向代理部署)。

### 本机覆盖层（端口 / 网络 / 容器别名）

`docker-compose.yml` 里的端口是 `"7863:7863"`，会监听 `0.0.0.0`。面板前面通常还有
一层反向代理负责 TLS 与鉴权，所以实际部署几乎总要覆盖这两点：只绑回环地址、
加入前置网关所在的 docker 网络、并给对方一个稳定的容器别名。

```bash
cp docker-compose.override.example.yml docker-compose.override.yml
# 按需改端口 / 网络名 / 别名，或写进同目录 .env
docker compose up -d
```

模板里的 `ports: !override` 是必需的：compose 默认把多份 `ports` **取并集**，
基础文件里的 `"7863:7863"` 会一起保留，等于照样裸奔在 `0.0.0.0`。加上 `!override`
才是整体替换（需要 compose ≥ 2.24）。

前置网关把渠道 `base_url` 指向本服务时要用**容器别名**（模板默认 `omnigate`，
可用 `PANEL_ALIAS` 改），不要用 `127.0.0.1`——容器内的回环是它自己。别名解析失败
的典型表现是上游报 `dial tcp 127.0.0.1:7863: connect: connection refused`。

真实的 `docker-compose.override.yml` 已加入 `.gitignore`：端口、网络名、别名都是
本机事实，不应该进版本库。

## 开发

```bash
go test ./...        # 单元测试
go vet ./...         # 静态检查
go build ./...       # 全量编译
```

前端没有构建步骤：直接修改 `internal/panel/web/` 下的 HTML/CSS/ES Module，重新
`go build` 即生效。浏览器打开 `/panel/?mock=1` 可使用内置 mock 数据预览页面。

已知的 Windows 测试问题：`internal/omnigate/imagegen` 的临时文件清理用例在
Windows 上可能因文件句柄未释放而报 cleanup 错误；在 Linux/macOS/CI 上正常。

## 从旧版本迁移

本分支不做兼容：

- 配置以 PostgreSQL 为唯一权威；`omnigate.json` 只在首次迁移时读取一次；
- `auths/`、`data/state.json`、旧 `config.json` 中的 WorkBuddy 字段不再读取；
- 旧 `/omni/*` 路径不再保留，网关直接在 `/v1/*`；
- 旧面板 URL 的 hash 路由（`#accounts` 等）不再映射。

升级前请先整目录备份；新版本首次启动会自动导入旧配置，并把原文件备份到
`data/backups/config-migration-*`。

## 反向代理部署

本服务是**流式**接口：长 prompt + 长输出的真实响应时间经常达到 30～60s 甚至更久
（实测有 156s 的正常请求）。因此放在 Nginx / OpenResty 后面时，`location` 里**必须**
放宽超时并关闭缓冲：

```nginx
location / {
    proxy_pass http://127.0.0.1:7863;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    # SSE 流式响应：关闭缓冲并放宽超时
    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;
}
```

改完校验并热加载：

```bash
nginx -t && nginx -s reload
# 确认实际加载的配置里包含这几行
nginx -T | grep -E 'proxy_read_timeout|proxy_send_timeout|proxy_buffering'
```

### 为什么必须这么做

nginx 的 `proxy_read_timeout` **默认只有 60s**。超过后反代会主动断开并返回
**504**，但此时上游（本服务）已经把请求处理完并产生了 token 消耗——客户端没拿到
结果，于是重试，**同一份 prompt 被反复计费**。

需要特别强调的是：**本服务自身从不返回 504**。代码里唯一的 `504` 字面量是
`internal/gemini/gemini.go` 的状态码→名称映射。`/v1/chat/completions` 只会返回：

| 状态码 | 含义 |
|---|---|
| `400` | 请求体读取失败 / JSON 非法 |
| `403` | 客户端密钥 scope 不允许该模型 |
| `502` | 上游错误，或上游返回空响应/空流 |
| `503` | 流式失败（仅用于请求记录的观测状态码，HTTP 头已发 200） |

所以**看到 504 一定是中间层（反代 / CDN）发出的**，不是本服务。

### 空回复不会让客户端无限重试

上游消耗了 token 却一个正文字节都没回，是真实存在的（典型成因：`max_tokens` 太小，
模型把预算全花在不上报为 `reasoning_content` 的内部推理上，于是只发空 delta 帧 +
`finish_reason=length`）。这类情况分成两种，本服务的处理方式不同：

| 上游行为 | 本服务返回 | 客户端应如何理解 |
|---|---|---|
| 200 + 连 `finish_reason` 都没发（0 有效帧） | `502` + `upstream_error`，并附上游原文片段 | 上游确实失败，不是空回复；换供应商 / 看上游原文 |
| 200 + 有 `finish_reason`，但正文为空 | `200` + 空正文 + 真实 `finish_reason` | 合法空补全（预算耗尽），应停止重试 |

关键点：**「合法空回复」返回的是 200，不是错误**。如果把它当故障返回 502，客户端会
按 5xx 语义自动重试，而每次重试都要重新计费一遍——同一份 prompt 被反复收费。
所以只要上游明确收尾了，这里就照 OpenAI 语义返回空内容；请求记录里状态是成功，
但日志会打一条 `空回复 … 上游正常收尾但无正文` 便于排查。

流的形态同理：空流会写一帧 `error` 并以 `502` 观测，但 HTTP 头早已是 200，客户端
看到的是流里的错误帧 + `[DONE]`，不会重连。

### 会话中途被掐断（`interrupted`）怎么定位

「对话跑到一半突然停了」在请求记录里长这样：`status=200`、`outcome=interrupted`、
`ttfb≈5ms`、`prompt_tokens` / `completion_tokens` 全是 0。**它不是本服务的故障，
而是链路上某一层先断开了连接**，本服务只是如实记录了「写不进去」。

#### 关键机制：SSE 注释帧穿不过 SSE 再编码层

本服务在上游长静默（扩展思考 / 长 prompt 预填充）时会发保活帧，避免中间层按读
超时掐断流。这里有一个**极易踩中的陷阱**：

| 保活帧形态 | 客户端直连本服务 | 中间还有一层 SSE 再编码器（如 new-api） |
|---|---|---|
| SSE 注释 `: ping` | ✅ 有效 | ❌ **被整行丢弃**，客户端在静默期收不到任何字节 |
| `data:` 空 delta chunk | ✅ 有效 | ✅ 有效（再编码层只转发 `data:` / `[DONE]` 行） |

new-api 的 `StreamScannerHandler` 明确只处理 `data:` 开头与 `[DONE]` 行：

```go
if data[:5] != "data:" && data[:6] != "[DONE]" { continue }
```

更隐蔽的是它还**延迟一帧转发**（为了从最后一帧里捞 usage）：

```go
if lastStreamData != "" {
    HandleStreamFormat(c, info, lastStreamData, ...)  // 写出的是「上一帧」
}
if len(data) > 0 {
    lastStreamData = data   // 当前帧先扣住
}
```

两个机制叠加才是完整的故障链：注释行被丢弃 → 扫描回调不触发 → 被扣住的帧永远发不出去。
所以保活不能只靠「偶尔发一帧」，必须**周期性持续供给**，让延迟转发一直有下一帧可等。

所以保活帧**必须是合法的 `data:` 帧**。本服务现在发的是带 `omnigate_keepalive`
标记的空 delta chunk：OpenAI 客户端会把它当无内容帧忽略，Anthropic / Responses /
Gemini 三个入站方言则各自翻译成该方言自己的心跳（`ping` 事件 /
`response.in_progress` / 空 `candidates`），因此三种协议都能穿透再编码层。

> 历史坑：早期实现用的是 `: ping` 注释帧。直连测试永远是好的，一旦前面挂了
> new-api 就不再生效——实测 24 小时内 96 次 `interrupted`，全是 200 + 零 token。
> 这类「本地测不出来」的问题只能靠在链路中间实测字节到达时刻来发现。

#### 判断是谁先断的

`interrupted` 记录现在带 `failure` 字段（面板详情「失败原因」直接显示），取值形如：

| `failure` 内容 | 含义 | 该查哪里 |
|---|---|---|
| `client canceled the request (context canceled)` | 客户端主动取消（关页面 / IDE 点停止） | 客户端行为，属正常 |
| `client closed the connection while writing: ...broken pipe` | 写的时候对端已关 | 客户端或中间层先断开 |
| `request deadline exceeded` | 本服务请求上下文超时 | 调用方设置的超时 |
| `upstream returned an empty stream · 上游原文: ...` | 上游 200 却 0 有效帧 | 看附带的原文（限流 / 审核 / 参数错） |

如果 `failure` 显示客户端主动取消、但你并没有取消，那说明**是中间层替客户端断的**
（例如反代的 `proxy_read_timeout`）。此时往上一层查：反代 access log 里应能看到
`499`（客户端断开）或 `504`（反代超时）。

#### 定位步骤

1. 查请求记录的 `failure`，先分清是「客户端取消」还是「写失败」。
2. 若指向中间层，看反代 access log 的 `499` / `504` 计数与时间点是否与记录吻合。
3. 确认反代的 `proxy_buffering off` 与 `proxy_read_timeout` 已按上一节配好。
4. **实测字节到达时刻**：直接对链路发一个长思考请求，记录 `data:` 帧的到达间隔。
   如果静默期超过 10s 却一个字节都没有，保活没有穿透——检查是不是又退回注释帧了。

### 排障速查

出现 504 时按这个顺序定位：

1. **看反代的 access log 状态码分布**，确认 504 是反代发出的。
2. **看反代的 error log**，找 `upstream timed out (110: Connection timed out)`，
   它会写明是哪个 upstream 超时。
3. **对比同一个后端不同 vhost 的 proxy 配置**。从静态站模板复制来的 `location`
   块很容易漏掉 SSE 相关的 `proxy_buffering` / `proxy_read_timeout`。
4. **用「取消传播方向」判断谁先超时**：如果上游侧日志出现
   `client_gone` / `context canceled`，说明是外层先断的，不是内层超时。

> 💡 改这类配置的两个坑：一是别用 `sed -i` 插入含引号的行（引号会被外层 shell
> 吃掉），整文件替换最稳；二是别用中文注释经 PowerShell/SSH 管道传输（编码会被
> 破坏，把下一行指令吞进注释里），用 ASCII 注释或直接 `scp` 传文件。

## License

MIT（见 `LICENSE`）。
