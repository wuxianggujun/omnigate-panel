# OmniGate Panel

单二进制、多上游、多账号的 OpenAI 兼容网关，内置 Web 管理面板。

当前版本已移除旧的 WorkBuddy/CodeBuddy 反代链路，只保留 **OmniGate 引擎**：
一个进程同时承载供应商、账号池、路由、签到、请求记录与面板。代码与配置均不向后
兼容旧版本，升级前请先备份旧目录。

## 特性

- **OpenAI / Anthropic / Gemini 兼容入口**：`/v1/chat/completions`、
  `/v1/responses`、`/v1/messages`、`/v1beta/models/*` 等。
- **多供应商**：raccoon、Runable、OpenCode、New API、任意 OpenAI 兼容上游、
  Gemini Web、TRAE。
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
- **请求**：请求归档、详情抽屉、运行日志。
- **系统**：客户端密钥、面板账号、出站代理、运行参数与存储。

字段表单由 `GET /panel/api/omni/provider-types` 返回的插件元数据驱动。新增
provider 时只需后端提供 descriptor，前端无需改动。

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

## License

MIT（见 `LICENSE`）。
