// Package plugin defines omnigate's in-process provider SPI.
//
// A provider plugin is three things:
//
//  1. a minimal Provider (an alias of provider.Provider) that actually speaks to
//     the upstream;
//  2. zero or more optional capability interfaces (account models, token
//     refresh, balance, actions, login, ...), asserted by the gateway at runtime;
//  3. one declarative Plugin descriptor registered through Register /
//     MustRegister, telling config validation and the panel which fields,
//     capabilities, actions and scheduled tasks the type supports.
//
// There is deliberately no implicit init()-time registration: the process that
// owns the provider set (main / gateway bootstrap) explicitly calls
// catalog.Register or registers its own descriptors. That keeps tests, build
// tags and embedded deployments in control of what exists.
//
// This is an in-process Go SPI only. A plugin cannot spawn an external process,
// load WASM, create database schema, or mount HTTP routes; those remain core
// concerns.
package plugin

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// Provider is the minimal upstream contract. It is an alias so existing
// provider implementations satisfy the SPI without importing this package.
type Provider = provider.Provider

// Aliases for the request/response types a Provider needs. They keep plugin
// authors on one import while remaining bit-identical to the provider package.
type (
	Account   = provider.Account
	ChatInput = provider.ChatInput
	Stream    = provider.Stream
	Event     = provider.Event
)

// AccountMode describes where a type's credentials live.
type AccountMode string

const (
	// AccountModeNone: no account records; credentials are provider-level keys.
	AccountModeNone AccountMode = "none"
	// AccountModePool: a static pool of account records (declared in config).
	AccountModePool AccountMode = "pool"
	// AccountModeDynamic: accounts are created at runtime by a login or capture
	// flow and persisted as dynamic accounts, in addition to any config entries.
	AccountModeDynamic AccountMode = "dynamic"
)

// Valid reports whether m is a known account mode.
func (m AccountMode) Valid() bool {
	switch m {
	case AccountModeNone, AccountModePool, AccountModeDynamic:
		return true
	}
	return false
}

// WireFormat names the protocol dialect a provider speaks.
type WireFormat string

const (
	WireOpenAI    WireFormat = "openai"
	WireRunable   WireFormat = "runable"
	WireGeminiWeb WireFormat = "gemini-web"
	WireTrae      WireFormat = "trae-solo"
)

// Valid reports whether w is a known wire format.
func (w WireFormat) Valid() bool {
	switch w {
	case WireOpenAI, WireRunable, WireGeminiWeb, WireTrae:
		return true
	}
	return false
}

// Traits captures protocol-level facts the gateway needs before it has a
// provider instance, so it no longer has to switch on the type string.
type Traits struct {
	WireFormat WireFormat `json:"wire_format"`
	// ToolEmulation: tools are advertised in the prompt and parsed from text.
	ToolEmulation bool `json:"tool_emulation,omitempty"`
	Attachments   bool `json:"attachments,omitempty"`
	// ReasoningEffort: the OpenAI thinking tier is forwarded upstream.
	ReasoningEffort bool `json:"reasoning_effort,omitempty"`
	// MaxPromptChars is the upstream's default single-prompt limit; 0 = none.
	MaxPromptChars int  `json:"max_prompt_chars,omitempty"`
	RotatesCookie  bool `json:"rotates_cookie,omitempty"`
	// AccountScopedModels: the model catalog requires an authenticated account.
	AccountScopedModels bool `json:"account_scoped_models,omitempty"`
	// SystemPrompt: the gateway injects provider.system_prompt for this type.
	SystemPrompt bool `json:"system_prompt,omitempty"`
}

// Capability is a coarse feature flag shown by the panel and used for
// pre-flight decisions. Fine-grained behavior belongs in Traits/interfaces.
type Capability string

const (
	CapAccounts       Capability = "accounts"
	CapUpstreamKeys   Capability = "upstream_keys"
	CapBalance        Capability = "balance"
	CapCheckin        Capability = "checkin"
	CapQuota          Capability = "quota"
	CapLogin          Capability = "login"
	CapFingerprint    Capability = "fingerprint"
	CapModels         Capability = "models"
	CapRefresh        Capability = "refresh"
	CapAttachments    Capability = "attachments"
	CapCookieRotation Capability = "cookie_rotation"
)

var capabilities = map[Capability]struct{}{
	CapAccounts:       {},
	CapUpstreamKeys:   {},
	CapBalance:        {},
	CapCheckin:        {},
	CapQuota:          {},
	CapLogin:          {},
	CapFingerprint:    {},
	CapModels:         {},
	CapRefresh:        {},
	CapAttachments:    {},
	CapCookieRotation: {},
}

// Valid reports whether c is a known capability.
func (c Capability) Valid() bool {
	_, ok := capabilities[c]
	return ok
}

// ActionName is one account/provider operation the panel may invoke.
type ActionName string

const (
	ActionCheckin ActionName = "checkin"
	ActionRefresh ActionName = "refresh"
	ActionVerify  ActionName = "verify"
	ActionQuota   ActionName = "quota"
	ActionRemove  ActionName = "remove"
	ActionLogin   ActionName = "login"
)

var actionNames = map[ActionName]struct{}{
	ActionCheckin: {},
	ActionRefresh: {},
	ActionVerify:  {},
	ActionQuota:   {},
	ActionRemove:  {},
	ActionLogin:   {},
}

// Valid reports whether a is a known action name.
func (a ActionName) Valid() bool {
	_, ok := actionNames[a]
	return ok
}

// ActionScope says whether an action operates on one account or the whole
// provider (for example, opencode quota is provider-scoped).
type ActionScope string

const (
	ActionScopeAccount  ActionScope = "account"
	ActionScopeProvider ActionScope = "provider"
)

// Valid reports whether s is a known action scope.
func (s ActionScope) Valid() bool {
	return s == ActionScopeAccount || s == ActionScopeProvider
}

// Action describes one panel-invokable operation.
type Action struct {
	Name    ActionName  `json:"name"`
	Label   string      `json:"label"`
	Help    string      `json:"help,omitempty"`
	Scope   ActionScope `json:"scope"`
	Confirm bool        `json:"confirm,omitempty"`
}

// Validate checks one action descriptor.
func (a Action) Validate() error {
	if !a.Name.Valid() {
		return fmt.Errorf("未知 action %q", a.Name)
	}
	if strings.TrimSpace(a.Label) == "" {
		return fmt.Errorf("action %q: label 不能为空", a.Name)
	}
	if !a.Scope.Valid() {
		return fmt.Errorf("action %q: 未知 scope %q", a.Name, a.Scope)
	}
	return nil
}

// Task describes a built-in scheduled task kind understood by the scheduler.
// Type matches config.CheckinTask.Type (for example "raccoon" or
// "newapi-refresh").
//
// Action is what makes the declaration live: the scheduler resolves a task type
// to its owning plugin through this descriptor and then invokes Action on that
// provider's behavior set, instead of switching on the type string. Empty Action
// means the task has no provider-side operation (it is not a maintenance task).
type Task struct {
	Type        string      `json:"type"`
	Label       string      `json:"label"`
	Description string      `json:"description,omitempty"`
	Action      ActionName  `json:"action,omitempty"`
	Scope       ActionScope `json:"scope,omitempty"`
}

// LoginKind names the interaction a type's login uses. It tells the panel which
// wizard to render; the transport (endpoints, polling) stays a panel concern so
// this package never depends on panel routes.
type LoginKind string

const (
	// LoginKindOAuth: open an upstream authorize URL in the browser, then capture
	// the callback (paste or automatic redirect back to the panel).
	LoginKindOAuth LoginKind = "oauth"
	// LoginKindSnippet: the user pastes a generated snippet into the upstream
	// site's console; the snippet posts the captured session back to the panel.
	LoginKindSnippet LoginKind = "snippet"
	// LoginKindPaste: the user pastes a credential (cookie / token) directly.
	LoginKindPaste LoginKind = "paste"
	// LoginKindManual: no guided flow; the operator fills account_schema by hand.
	LoginKindManual LoginKind = "manual"
)

// Valid reports whether k is a known login kind.
func (k LoginKind) Valid() bool {
	switch k {
	case LoginKindOAuth, LoginKindSnippet, LoginKindPaste, LoginKindManual:
		return true
	}
	return false
}

// LoginTransport names the panel endpoint family that drives a login flow. It is
// what keeps the panel from switching on the provider type: the wizard reads Kind
// (what the user does) and Transport (where the panel talks) from the descriptor.
type LoginTransport string

const (
	// LoginTransportAdmin routes through the panel's /omni/admin/<type>/* proxy to
	// the in-process gateway admin endpoints (authorize / callback / token).
	LoginTransportAdmin LoginTransport = "admin"
	// LoginTransportAccount uses the panel's neutral /omni/account/login endpoint,
	// which verifies the pasted credential server-side and saves the account.
	LoginTransportAccount LoginTransport = "account"
	// LoginTransportNewAPI uses the dedicated /omni/newapi/* capture endpoints
	// (authorize → console snippet → status poll).
	LoginTransportNewAPI LoginTransport = "newapi"
)

// Valid reports whether t is a known transport.
func (t LoginTransport) Valid() bool {
	switch t {
	case LoginTransportAdmin, LoginTransportAccount, LoginTransportNewAPI:
		return true
	}
	return false
}

// LoginSpec declares how accounts of a type are created interactively. It is
// panel-facing metadata: the wizard collects Fields (a subset of the account
// schema plus flow-specific inputs), then drives the flow identified by Kind
// over the endpoint family named by Transport.
type LoginSpec struct {
	Kind      LoginKind      `json:"kind"`
	Transport LoginTransport `json:"transport"`
	Label     string         `json:"label,omitempty"`
	Help      string         `json:"help,omitempty"`
	// Fields are the inputs the wizard collects before starting the flow. They
	// reuse the Field contract so the panel renders them with the same controls.
	Fields []Field `json:"fields,omitempty"`
}

// Plugin is the declarative half of a provider plugin. The runtime half is the
// Provider plus optional interfaces, created by Factory.
type Plugin struct {
	Type        string      `json:"type"`
	DisplayName string      `json:"display_name"`
	Description string      `json:"description,omitempty"`
	AccountMode AccountMode `json:"account_mode"`
	Traits      Traits      `json:"traits"`

	// ConfigFields / AccountFields are serialized as config_schema and
	// account_schema for direct panel consumption.
	ConfigFields  []Field `json:"config_schema"`
	AccountFields []Field `json:"account_schema"`

	Actions      []Action     `json:"actions"`
	Tasks        []Task       `json:"tasks"`
	Capabilities []Capability `json:"capabilities"`

	// Login declares the interactive account-creation flow, if any. Nil means
	// accounts are hand-written (account_schema only). Kept separate from
	// Actions: an action is a repeatable operation on an existing account,
	// whereas Login creates one.
	Login *LoginSpec `json:"login,omitempty"`

	// Factory builds the runtime provider. It is deliberately excluded from
	// JSON: the panel only consumes metadata, never constructors.
	Factory Factory `json:"-"`
}

// Deps carries runtime dependencies a Factory may need. It intentionally
// exposes only data-dir and logging; provider-specific stores are configured
// by the core after construction via optional interfaces.
type Deps struct {
	DataDir string
	Logger  func(format string, args ...any)
}

// ProviderSpec is the provider-level configuration passed to a Factory. It is
// a neutral copy of config.ProviderConfig's cross-provider fields; provider
// packages do not import the config package (avoids an import cycle).
type ProviderSpec struct {
	Name         string
	BaseURL      string
	DeviceHeader string
	APIKey       string
	APIKeys      []string
	Models       []string
	ZenKeys      []string
	GoKeys       []string
	TierPrefer   string
	GoBaseURL    string
	Anonymous    bool
	MainOrigin   string
	LLMBase      string
	AuthBase     string
	AgentHost    string
	UgHost       string
	OAuthHost    string
}

// Factory constructs a Provider from spec.
type Factory func(ProviderSpec, Deps) (Provider, error)

var pluginTypeRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Validate checks the descriptor's structural invariants. It does not check
// that the runtime Provider exists; registration is metadata-only.
func (p Plugin) Validate() error {
	if err := p.validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPlugin, err)
	}
	return nil
}

func (p Plugin) validate() error {
	if !pluginTypeRE.MatchString(p.Type) {
		return fmt.Errorf("type %q 非法（应为小写字母开头的 [a-z0-9_-]+）", p.Type)
	}
	if strings.TrimSpace(p.DisplayName) == "" {
		return fmt.Errorf("type %q: display_name 不能为空", p.Type)
	}
	if !p.AccountMode.Valid() {
		return fmt.Errorf("type %q: 未知 account_mode %q", p.Type, p.AccountMode)
	}
	if !p.Traits.WireFormat.Valid() {
		return fmt.Errorf("type %q: 未知 wire_format %q", p.Type, p.Traits.WireFormat)
	}
	if p.Traits.MaxPromptChars < 0 {
		return fmt.Errorf("type %q: max_prompt_chars 不能为负", p.Type)
	}
	if err := validateFields("config_schema", p.ConfigFields); err != nil {
		return fmt.Errorf("type %q: %w", p.Type, err)
	}
	if err := validateFields("account_schema", p.AccountFields); err != nil {
		return fmt.Errorf("type %q: %w", p.Type, err)
	}
	switch p.AccountMode {
	case AccountModeNone:
		if len(p.AccountFields) > 0 {
			return fmt.Errorf("type %q: account_mode=none 不应声明 account_schema", p.Type)
		}
	default:
		if len(p.AccountFields) == 0 {
			return fmt.Errorf("type %q: account_mode=%s 必须声明 account_schema", p.Type, p.AccountMode)
		}
	}

	caps := make(map[Capability]struct{}, len(p.Capabilities))
	for _, c := range p.Capabilities {
		if !c.Valid() {
			return fmt.Errorf("type %q: 未知 capability %q", p.Type, c)
		}
		if _, dup := caps[c]; dup {
			return fmt.Errorf("type %q: capability %q 重复", p.Type, c)
		}
		caps[c] = struct{}{}
	}

	actions := make(map[ActionName]struct{}, len(p.Actions))
	for _, a := range p.Actions {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("type %q: %w", p.Type, err)
		}
		if _, dup := actions[a.Name]; dup {
			return fmt.Errorf("type %q: action %q 重复", p.Type, a.Name)
		}
		actions[a.Name] = struct{}{}
	}

	tasks := make(map[string]struct{}, len(p.Tasks))
	for _, task := range p.Tasks {
		if task.Type == "" {
			return fmt.Errorf("type %q: task.type 不能为空", p.Type)
		}
		if strings.TrimSpace(task.Label) == "" {
			return fmt.Errorf("type %q: task %q 缺少 label", p.Type, task.Type)
		}
		if _, dup := tasks[task.Type]; dup {
			return fmt.Errorf("type %q: task %q 重复", p.Type, task.Type)
		}
		// A scheduled task that runs a provider action must name one the type
		// actually declares; otherwise the scheduler would dispatch into an
		// operation the provider rejects at runtime.
		if task.Action != "" {
			if !task.Action.Valid() {
				return fmt.Errorf("type %q: task %q: 未知 action %q", p.Type, task.Type, task.Action)
			}
			if _, declared := actions[task.Action]; !declared {
				return fmt.Errorf("type %q: task %q 的 action %q 未在 actions 中声明", p.Type, task.Type, task.Action)
			}
		}
		if task.Scope != "" && !task.Scope.Valid() {
			return fmt.Errorf("type %q: task %q: 未知 scope %q", p.Type, task.Type, task.Scope)
		}
		tasks[task.Type] = struct{}{}
	}

	if p.Login != nil {
		if !p.Login.Kind.Valid() {
			return fmt.Errorf("type %q: 未知 login.kind %q", p.Type, p.Login.Kind)
		}
		if !p.Login.Transport.Valid() {
			return fmt.Errorf("type %q: 未知 login.transport %q", p.Type, p.Login.Transport)
		}
		if p.AccountMode == AccountModeNone {
			return fmt.Errorf("type %q: account_mode=none 不应声明 login", p.Type)
		}
		if err := validateFields("login.fields", p.Login.Fields); err != nil {
			return fmt.Errorf("type %q: %w", p.Type, err)
		}
	}
	return nil
}

// HasCapability reports whether the descriptor declares c.
func (p Plugin) HasCapability(c Capability) bool {
	for _, got := range p.Capabilities {
		if got == c {
			return true
		}
	}
	return false
}

// HasAction reports whether the descriptor declares the named action.
func (p Plugin) HasAction(name ActionName) bool {
	for _, a := range p.Actions {
		if a.Name == name {
			return true
		}
	}
	return false
}

// AccountModelLister is an optional capability: the catalog needs a live
// account (raccoon/newapi/trae). Re-exported from provider.
type AccountModelLister = provider.AccountModelLister

// CookieRotator is an optional capability: the upstream rotates the account
// cookie on responses (geminiweb). Re-exported from provider.
type CookieRotator = provider.CookieRotator

// TokenRefresher is implemented by token-based providers that can mint a fresh
// access token from a refresh token.
type TokenRefresher interface {
	TokenExpired(acc *Account) bool
	RefreshAccount(ctx context.Context, acc *Account) error
	Unauthorized(err error) bool
}

// Balance is a provider-neutral account balance snapshot.
type Balance struct {
	Available float64        `json:"available"`
	Total     float64        `json:"total,omitempty"`
	Unit      string         `json:"unit,omitempty"`
	Message   string         `json:"message,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// BalanceReader is an optional capability for providers with a queryable
// balance (raccoon/runable/trae/newapi).
type BalanceReader interface {
	QueryBalance(ctx context.Context, acc *Account) (*Balance, error)
}

// ActionRequest is one invocation of a declared Action. Account is nil for a
// provider-scoped action such as opencode quota.
type ActionRequest struct {
	Action  ActionName
	Account *Account
	Params  map[string]string
}

// ActionResult is the provider-neutral outcome of an action.
type ActionResult struct {
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
}

// ActionHandler is an optional capability for providers that execute named
// panel actions themselves. Providers with only a few built-in operations may
// expose them through AccountOps instead.
type ActionHandler interface {
	HandleAction(ctx context.Context, req ActionRequest) (ActionResult, error)
}

// AccountStatus is the outcome of an account verification.
type AccountStatus struct {
	Label     string   `json:"label,omitempty"`
	Valid     bool     `json:"valid"`
	ExpiresAt int64    `json:"expires_at,omitempty"`
	Message   string   `json:"message,omitempty"`
	Models    []string `json:"models,omitempty"`
}

// AccountOutcome is one account's result from a batch operation (check-in,
// credential refresh). It is provider-neutral so the panel renders every type
// with one component; the JSON shape is stable and shared by all providers.
type AccountOutcome struct {
	Label   string `json:"label"`
	Success bool   `json:"success"`
	Message string `json:"msg,omitempty"`
	Error   string `json:"error,omitempty"`
	// AlreadyClaimed marks "nothing to do" (already checked in today) as a
	// neutral outcome rather than a failure.
	AlreadyClaimed bool `json:"already_claimed,omitempty"`
}

// AccountOps is an optional capability for account-scoped maintenance that is
// not a chat turn: check-in (or session probe for geminiweb) and verification.
// Account creation/removal stays a gateway persistence concern.
type AccountOps interface {
	Checkin(ctx context.Context, acc *Account) (ActionResult, error)
	Verify(ctx context.Context, acc *Account) (*AccountStatus, error)
}

// LoginRequest carries the account schema values collected by the panel.
// Fields is keyed by Field.Key.
type LoginRequest struct {
	Label  string
	Fields map[string]string
}

// LoginResult describes either a finished login (Account set) or a multi-step
// flow the panel must continue (AuthorizeURL / State set). A provider that needs
// a callback can accept the callback parameters in a later LoginRequest.
type LoginResult struct {
	Account      *Account `json:"-"`
	AuthorizeURL string   `json:"authorize_url,omitempty"`
	State        string   `json:"state,omitempty"`
	Message      string   `json:"message,omitempty"`
}

// LoginFlow is an optional capability for providers whose accounts are created
// by an interactive login rather than hand-written config.
type LoginFlow interface {
	Login(ctx context.Context, req LoginRequest) (*LoginResult, error)
}

// ModelResolver is an optional capability for mapping an exposed model alias to
// the upstream model (geminiweb custom Gems, alias tables).
type ModelResolver interface {
	ResolveModel(model string) (upstream string, ok bool)
}

// ConfigValidator is an optional capability for semantic config checks that a
// generic field schema cannot express. raw is the provider config rendered as a
// map so this package does not depend on config (config will depend on plugin).
type ConfigValidator interface {
	ValidateConfig(raw map[string]any) error
}
