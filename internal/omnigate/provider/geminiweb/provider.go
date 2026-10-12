package geminiweb

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/imageintent"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/imagegen"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

// session is the live per-account state derived from an account's Google
// cookies: the cookie jar (with rotations), the SNlM0e token, the upload push
// id, and the account's real per-family models.
type session struct {
	jar          *cookieJar
	sessionToken string
	pushID       string
	sessionUUID  string
	familyModel  map[string]string
	healthy      bool
	lastErr      string
	// source is the raw cookie string the session was built from; a changed
	// cookie (re-login) invalidates the cached session.
	source string
}

// Provider implements provider.Provider for Gemini Web.
type Provider struct {
	name   string
	client *Client
	models []string
	// images keeps AI-generated images on disk so a reply can link to them
	// instead of inlining megabytes of base64. Nil = inline as a data URI.
	images *imagegen.Store

	mu       sync.Mutex
	sessions map[string]*session
}

// 编译期保证：可选能力都挂在 Provider 上（网关用类型断言取）。
var (
	_ provider.CookieRotator = (*Provider)(nil)
)

// NewProvider builds a Gemini Web provider. models overrides the exposed catalog
// when non-empty; the default is the stable public names.
func NewProvider(name string, models []string) *Provider {
	if len(models) == 0 {
		models = append([]string(nil), publicModels...)
	}
	return &Provider{
		name:     name,
		client:   NewClient(),
		models:   models,
		sessions: map[string]*session{},
	}
}

// Name returns the provider name.
func (p *Provider) Name() string { return p.name }

// Type returns the provider type.
func (p *Provider) Type() string { return "geminiweb" }

// SetProxy routes outbound requests through u (nil = direct).
func (p *Provider) SetProxy(u *url.URL) { p.client.SetProxy(u) }

// SetProxySelector routes outbound requests through the proxy pool.
func (p *Provider) SetProxySelector(sel outbound.Selector) { p.client.SetProxySelector(sel) }

// SetImageStore attaches the store used to persist AI-generated images. Without
// one a generated image is inlined as a data URI (or dropped when there is no
// public base URL to link to).
func (p *Provider) SetImageStore(s *imagegen.Store) { p.images = s }

// RotatesCookie marks this provider as one whose credential cookie the upstream
// rotates; the gateway persists acc.Cookie after each call so a restart keeps it.
func (p *Provider) RotatesCookie() bool { return true }

// ListModels returns the stable public catalog (Gemini Web exposes no per-key
// model endpoint).
func (p *Provider) ListModels(ctx context.Context) ([]openai.Model, error) {
	return p.catalog(), nil
}

func (p *Provider) catalog() []openai.Model {
	out := make([]openai.Model, 0, len(p.models))
	for _, id := range p.models {
		out = append(out, openai.Model{ID: id, Object: "model", OwnedBy: p.name})
	}
	return out
}

// StreamChat sends one turn and streams the reply.
func (p *Provider) StreamChat(ctx context.Context, acc *provider.Account, in provider.ChatInput) (provider.Stream, error) {
	if acc == nil {
		return nil, &UpstreamError{Status: 401, Message: "账号为空"}
	}
	s, err := p.ensureSession(ctx, acc)
	if err != nil {
		return nil, err
	}
	model := resolveModel(in.Model, s.familyModel)
	thinking := strings.TrimSpace(in.ReasoningEffort) != ""
	resp, err := p.client.generate(ctx, s.jar, s.sessionToken, s.sessionUUID, in.Prompt, model, "", thinking)
	if err != nil {
		if Unauthorized(err) {
			p.markUnhealthy(acc, err)
		}
		return nil, err
	}
	// Persist any cookie rotation the upstream attached to this response.
	acc.Cookie = s.jar.header()
	// An image-generation turn is buffered: the generated image's link has to
	// precede the text, and the image is only known once the upstream finishes.
	return newWebStream(ctx, p.client, resp.Body, webStreamOpts{
		buffer:    imageintent.MaybeImageGeneration(in.Prompt),
		store:     p.images,
		imageBase: provider.ImageBase(ctx),
		jar:       s.jar,
	}), nil
}

// TokenExpired reports whether the account has no healthy session for its current
// cookie, which the gateway answers with a refresh.
func (p *Provider) TokenExpired(acc *provider.Account) bool {
	if acc == nil || strings.TrimSpace(acc.Cookie) == "" {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[p.sessionKey(acc)]
	return s == nil || !s.healthy || s.source != strings.TrimSpace(acc.Cookie)
}

// RefreshAccount rebuilds the account's session from its stored cookies.
func (p *Provider) RefreshAccount(ctx context.Context, acc *provider.Account) error {
	_, err := p.ensureSession(ctx, acc)
	return err
}

// Unauthorized reports whether err is an auth failure worth a refresh + retry.
func (p *Provider) Unauthorized(err error) bool { return Unauthorized(err) }

// CheckResult is the outcome of a session probe.
type CheckResult struct {
	Valid  bool     `json:"valid"`
	Models []string `json:"models,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// CheckAccount (re)builds the account's session and reports whether its cookies
// still work.
func (p *Provider) CheckAccount(ctx context.Context, acc *provider.Account) *CheckResult {
	s, err := p.ensureSession(ctx, acc)
	if err != nil {
		return &CheckResult{Valid: false, Error: err.Error()}
	}
	models := make([]string, 0, len(s.familyModel))
	for fam, m := range s.familyModel {
		models = append(models, fam+"="+m)
	}
	sort.Strings(models)
	return &CheckResult{Valid: true, Models: models}
}

// ResetAccount drops any cached session for a label (used after removal).
func (p *Provider) ResetAccount(label string) {
	p.mu.Lock()
	delete(p.sessions, label)
	p.mu.Unlock()
}

// ---------------------------------------------------------------------------

// ensureSession returns a healthy session for acc, (re)building it when missing,
// stale, or built from a different cookie.
func (p *Provider) ensureSession(ctx context.Context, acc *provider.Account) (*session, error) {
	raw := strings.TrimSpace(acc.Cookie)
	if raw == "" {
		return nil, &UpstreamError{Status: 401, Message: "该账号未配置 Cookie"}
	}
	key := p.sessionKey(acc)

	p.mu.Lock()
	prev := p.sessions[key]
	if prev != nil && prev.source == raw && prev.healthy {
		p.mu.Unlock()
		return prev, nil
	}
	p.mu.Unlock()

	// source is the canonical cookie string the session was built from. The jar
	// re-serializes sorted, so comparing against the caller's raw string would
	// differ even when nothing rotated and force a re-handshake every request;
	// canonicalize both sides instead.
	jar := parseCookies(raw)
	// A stale __Secure-1PSIDTS is the usual cause of a 401 on an otherwise valid
	// cookie set: rotate it on demand (the same call the browser makes) rather
	// than making the user paste cookies again. Not a background keep-alive loop.
	if prev != nil && prev.source == raw && !prev.healthy {
		_ = p.client.rotateCookies(ctx, jar)
	}
	s := &session{jar: jar, sessionUUID: newUUID(), source: jar.header()}
	token, push, err := p.client.acquire(ctx, s.jar)
	if err != nil {
		s.lastErr = err.Error()
		p.store(key, s)
		return nil, err
	}
	s.sessionToken = token
	s.pushID = push
	if fm, serr := p.client.status(ctx, s.jar, token); serr == nil && len(fm) > 0 {
		s.familyModel = fm
	}
	s.healthy = true
	p.store(key, s)
	acc.Cookie = s.jar.header()
	return s, nil
}

func (p *Provider) markUnhealthy(acc *provider.Account, err error) {
	key := p.sessionKey(acc)
	p.mu.Lock()
	if s := p.sessions[key]; s != nil {
		s.healthy = false
		s.lastErr = err.Error()
	}
	p.mu.Unlock()
}

func (p *Provider) store(key string, s *session) {
	p.mu.Lock()
	p.sessions[key] = s
	p.mu.Unlock()
}

func (p *Provider) sessionKey(acc *provider.Account) string {
	if acc.Label != "" {
		return acc.Label
	}
	return acc.Cookie
}

// newUUID returns an upper-case v4 UUID (the frontend sends one in the
// extended-thinking headers).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}
