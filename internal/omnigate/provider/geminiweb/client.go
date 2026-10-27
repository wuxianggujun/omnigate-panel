package geminiweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb/fingerprint"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

// UpstreamError carries an HTTP failure from Gemini so the gateway can tell an
// auth failure from a generic one.
type UpstreamError struct {
	Status  int
	Message string
}

func (e *UpstreamError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("gemini 返回 %d: %s", e.Status, e.Message)
	}
	return e.Message
}

// UpstreamStatus exposes the HTTP status to the gateway's route-failure classifier.
func (e *UpstreamError) UpstreamStatus() int { return e.Status }

// Unauthorized reports whether err looks like an expired or invalid session,
// which the gateway answers with a refresh + retry.
func Unauthorized(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Status == 401 || ue.Status == 403
	}
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "cookie") ||
		strings.Contains(s, "not ready") ||
		strings.Contains(s, "snlm0e") ||
		strings.Contains(s, "unauthorized")
}

var (
	snlm0eRE = regexp.MustCompile(`"SNlM0e":"([^"]+)"`)
	pushIDRE = regexp.MustCompile(`feeds/([a-zA-Z0-9_-]+)`)
)

// Client talks to the Gemini Web frontend endpoints.
type Client struct {
	http *http.Client
	// base is the concrete transport that SetProxy / SetProxySelector mutate and
	// that the TLS impersonator attaches to; http wraps it (decompression layer,
	// and the proxy-pool failover wrapper when a selector is installed).
	base   *http.Transport
	dialer *net.Dialer
	// fp is the anti-detection runtime (header builder / TLS impersonator /
	// jitter / cookie persistence). nil falls back to a shared in-memory default.
	fp *fingerprint.Runtime
	// uploadURL overrides where attachments are posted; empty means
	// contentPushURL. It is a field so a test can point the upload at a local
	// server without touching the real endpoint.
	uploadURL string
	// batchURL overrides the batchexecute endpoint; empty means batchexecuteURL.
	// It is a field so a test can point the RPCs (status / gems / chats) at a
	// local server without touching the real endpoint.
	batchURL string
}

// defaultFP is the shared fallback runtime used when a Client was built without
// one (e.g. a bare &Client{} in a test, or before SetFingerprint). It carries the
// built-in fingerprint, jitter off, TLS off, and no persistence.
var (
	defaultFPOnce sync.Once
	defaultFP     *fingerprint.Runtime
)

func defaultRuntime() *fingerprint.Runtime {
	defaultFPOnce.Do(func() {
		defaultFP = fingerprint.NewRuntime(fingerprint.Options{})
	})
	return defaultFP
}

// runtime returns the client's fingerprint runtime, falling back to the shared
// default so callers never have to nil-check.
func (c *Client) runtime() *fingerprint.Runtime {
	if c != nil && c.fp != nil {
		return c.fp
	}
	return defaultRuntime()
}

// SetFingerprint attaches an anti-detection runtime: its header builder drives
// every request's headers, its TLS impersonator (when enabled) takes over the
// direct HTTPS handshake, and its jitter paces the requests. A nil runtime (or
// one with TLS disabled) leaves the transport's standard handshake in place.
func (c *Client) SetFingerprint(rt *fingerprint.Runtime) {
	if c == nil {
		return
	}
	c.fp = rt
	if rt == nil || c.base == nil {
		return
	}
	rt.TLS().Apply(c.base, c.dialer)
	c.base.CloseIdleConnections()
}

// uploadEndpoint is the attachment upload URL.
func (c *Client) uploadEndpoint() string {
	if c.uploadURL != "" {
		return c.uploadURL
	}
	return contentPushURL
}

// batchEndpoint is the batchexecute RPC URL.
func (c *Client) batchEndpoint() string {
	if c.batchURL != "" {
		return c.batchURL
	}
	return batchexecuteURL
}

// NewClient builds a client with a direct transport; SetProxy / SetProxySelector
// re-route it, and SetFingerprint layers the anti-detection runtime on top.
func NewClient() *Client {
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	base := &http.Transport{
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: 180 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	c := &Client{base: base, dialer: dialer, fp: defaultRuntime()}
	c.rebuildHTTP()
	return c
}

// rebuildHTTP rebuilds the outer http.Client so it wraps the current base
// transport with the decompression layer. The fingerprint headers advertise
// br/zstd, so Go will not auto-decompress and the wrapper must.
func (c *Client) rebuildHTTP() {
	if c == nil || c.base == nil {
		return
	}
	c.http = &http.Client{
		Transport: fingerprint.DecompressTransport{Base: c.base},
	}
}

// SetProxy routes this client's outbound requests through u (nil = direct).
func (c *Client) SetProxy(u *url.URL) {
	if c == nil || c.base == nil || u == nil {
		return
	}
	c.base.Proxy = http.ProxyURL(u)
	c.base.CloseIdleConnections()
}

// SetProxySelector routes this client's outbound requests through the proxy pool
// selected per request by sel (nil = direct).
func (c *Client) SetProxySelector(sel outbound.Selector) {
	if c == nil || c.base == nil || sel == nil {
		return
	}
	if c.http == nil {
		c.rebuildHTTP()
	}
	c.http.Transport = fingerprint.DecompressTransport{Base: outbound.WrapTransport(c.base, sel)}
}

// setNavHeaders stamps a document navigation (the Google → Gemini handshake)
// using the fingerprint header builder, so UA / Sec-CH-UA / Sec-Fetch-* stay
// consistent with the configured Chrome version.
func (c *Client) setNavHeaders(req *http.Request) {
	c.runtime().Headers().Apply(req, req.Method, "")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
}

// setXHRHeaders stamps a same-origin XHR (the batchexecute / StreamGenerate
// calls). accept overrides the default Accept when non-empty.
func (c *Client) setXHRHeaders(req *http.Request, accept string) {
	c.runtime().Headers().Apply(req, req.Method, "")
	if accept == "" {
		accept = "*/*"
	}
	req.Header.Set("Accept", accept)
}

// pace applies the configured inter-request jitter for a scenario profile
// (navigation / api_call / cookie_rotate). It is a no-op when jitter is off.
func (c *Client) pace(ctx context.Context, profile string) {
	c.runtime().Jitter().Apply(ctx, profile)
}

// ---------------------------------------------------------------------------
// protocol operations

// acquire walks the Google → Gemini handshake, collecting rotated cookies and
// extracting the SNlM0e token and upload push id the generate endpoint needs.
func (c *Client) acquire(ctx context.Context, jar *fingerprint.Jar) (token, pushID string, err error) {
	var body string
	for _, u := range []string{googleHomeURL, geminiHomeURL, geminiAppENURL} {
		body, err = c.getPage(ctx, jar, u)
		if err != nil {
			return "", "", err
		}
	}
	if m := snlm0eRE.FindStringSubmatch(body); m != nil {
		push := defaultPushID
		if pm := pushIDRE.FindStringSubmatch(body); pm != nil {
			push = "feeds/" + pm[1]
		}
		return m[1], push, nil
	}
	if strings.Contains(body, "accounts.google.com") || strings.Contains(body, "ServiceLogin") {
		return "", "", &UpstreamError{Status: 401, Message: "Cookie 已失效，请重新登录"}
	}
	if len(body) < 1000 {
		return "", "", fmt.Errorf("gemini 返回异常（正文仅 %d 字节）", len(body))
	}
	return "", "", &UpstreamError{Status: 401, Message: "未找到 SNlM0e（Cookie 可能无效或已过期）"}
}

// getPage fetches one HTML page, merging rotated cookies into jar.
func (c *Client) getPage(ctx context.Context, jar *fingerprint.Jar, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	c.setNavHeaders(req)
	if h := jar.CookieHeader(); h != "" {
		req.Header.Set("Cookie", h)
	}
	c.pace(ctx, "navigation")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	jar.UpdateFromResponse(resp)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return "", &UpstreamError{Status: resp.StatusCode, Message: clip(string(raw))}
	}
	return string(raw), nil
}

// rotateCookies asks Google to rotate __Secure-1PSIDTS. This is ordinary
// credential maintenance (the same call the browser makes), invoked on demand
// when a session has gone stale — not a background keep-alive loop.
func (c *Client) rotateCookies(ctx context.Context, jar *fingerprint.Jar) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rotateCookiesURL,
		strings.NewReader(`[000,"-0000000000000000000"]`))
	if err != nil {
		return err
	}
	c.setXHRHeaders(req, "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://accounts.google.com")
	if h := jar.CookieHeader(); h != "" {
		req.Header.Set("Cookie", h)
	}
	c.pace(ctx, "cookie_rotate")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	jar.UpdateFromResponse(resp)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &UpstreamError{Status: resp.StatusCode, Message: "RotateCookies 失败"}
	}
	return nil
}

// status calls otAQ7b (GetUserStatus) to learn which concrete models the account
// currently has, keyed by family.
func (c *Client) status(ctx context.Context, jar *fingerprint.Jar, sessionToken string) (map[string]string, error) {
	raw, err := c.batchexecute(ctx, jar, sessionToken, "otAQ7b", "[]")
	if err != nil {
		return nil, err
	}
	return parseModelsFromStatus(raw), nil
}

// batchexecute performs one generic RPC, reusing the heartbeat URL params and
// headers template.
func (c *Client) batchexecute(ctx context.Context, jar *fingerprint.Jar, sessionToken, rpcID, payload string) (string, error) {
	if strings.TrimSpace(sessionToken) == "" {
		return "", &UpstreamError{Status: 401, Message: "缺少会话 token"}
	}
	inner, _ := json.Marshal([]any{[]any{[]any{rpcID, payload, nil, "generic"}}})
	form := url.Values{}
	form.Set("f.req", string(inner))
	form.Set("at", sessionToken)
	q := url.Values{}
	q.Set("rpcids", rpcID)
	q.Set("hl", "en")
	q.Set("_reqid", strconv.Itoa(randReqID()))
	q.Set("rt", "c")
	q.Set("source-path", "/app")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.batchEndpoint()+"?"+q.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	c.setXHRHeaders(req, "*/*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("x-goog-ext-525001261-jspb", "[1,null,null,null,null,null,null,null,[4]]")
	req.Header.Set("x-goog-ext-73010989-jspb", "[0]")
	req.Header.Set("X-Same-Domain", "1")
	if h := jar.CookieHeader(); h != "" {
		req.Header.Set("Cookie", h)
	}
	c.pace(ctx, "api_call")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	jar.UpdateFromResponse(resp)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return "", &UpstreamError{Status: resp.StatusCode, Message: clip(string(raw))}
	}
	return string(raw), nil
}

// generate posts one turn to StreamGenerate and returns the raw response so the
// caller can stream it. gemID (non-empty) runs the turn on a saved custom Gem.
func (c *Client) generate(ctx context.Context, jar *fingerprint.Jar, sessionToken, sessionUUID, prompt, model, convID string, files []fileRef, gemID string, thinking bool) (*http.Response, error) {
	if strings.TrimSpace(sessionToken) == "" {
		return nil, &UpstreamError{Status: 401, Message: "缺少会话 token"}
	}
	var encoded string
	extra := map[string]string{}
	if thinking {
		encoded = encodePayloadThinking(prompt, model, convID, files, gemID, sessionUUID)
		for k, v := range buildModelHeaderThinking(model, sessionUUID) {
			extra[k] = v
		}
		extra["x-goog-ext-525005358-jspb"] = `["` + sessionUUID + `",1]`
	} else {
		encoded = encodePayload(prompt, model, convID, files, gemID)
		for k, v := range buildModelHeader(model) {
			extra[k] = v
		}
	}
	form := url.Values{}
	form.Set("at", sessionToken)
	form.Set("f.req", encoded)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, generateURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	c.setXHRHeaders(req, "*/*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Same-Domain", "1")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	if h := jar.CookieHeader(); h != "" {
		req.Header.Set("Cookie", h)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	jar.UpdateFromResponse(resp)
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &UpstreamError{Status: resp.StatusCode, Message: clip(string(raw))}
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// generated-image download

// Image download tuning. A bare lh3 URL only yields a compressed thumbnail, so a
// size suffix is appended; 2048px is enough for a preview at a fraction of the
// bytes of full resolution.
const (
	imageSizeSuffix    = "=s2048"
	imageDownloadLimit = 32 << 20 // bytes
	imageReferer       = "https://gemini.google.com/"
)

// downloadImage fetches one generated image's bytes and returns them with the
// upstream's content type (empty when the image could not be had).
//
// lh3 URLs 302 through lh3→fife→lh3 and the cookie is dropped on the
// cross-domain hop, which turns the followed request into a 403. So the first
// hop is taken *without* following redirects, and its Location is re-fetched with
// the cookie.
func (c *Client) downloadImage(ctx context.Context, jar *fingerprint.Jar, rawURL string) ([]byte, string) {
	tail := rawURL
	if i := strings.LastIndex(rawURL, "/"); i >= 0 {
		tail = rawURL[i+1:]
	}
	full := rawURL
	if !strings.Contains(tail, "=s") && !strings.Contains(tail, "=w") {
		full += imageSizeSuffix
	}

	resp, err := c.imageRequest(ctx, jar, full, false)
	if err != nil {
		return nil, ""
	}
	// Captured before the body is drained: a 302 carries the next hop here and
	// no image bytes.
	loc := resp.Header.Get("Location")
	if body, mime := readImageResponse(resp); len(body) > 0 {
		return body, mime
	}
	if loc == "" {
		return nil, ""
	}
	resp2, err := c.imageRequest(ctx, jar, loc, true)
	if err != nil {
		return nil, ""
	}
	return readImageResponse(resp2)
}

// imageRequest issues one GET for an image. follow=false returns the first
// redirect instead of chasing it (see downloadImage).
func (c *Client) imageRequest(ctx context.Context, jar *fingerprint.Jar, u string, follow bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", provider.BrowserUA)
	req.Header.Set("Accept-Language", provider.AcceptLanguage)
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	req.Header.Set("Referer", imageReferer)
	if h := jar.CookieHeader(); h != "" {
		req.Header.Set("Cookie", h)
	}
	if follow {
		return c.http.Do(req)
	}
	// Copy the client so the no-redirect rule applies to this request only; the
	// Transport (and therefore the proxy configuration) is shared.
	noRedirect := *c.http
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return noRedirect.Do(req)
}

// readImageResponse consumes resp and returns its bytes when it really is an
// image. The body is always closed and drained so the connection is reused.
func readImageResponse(resp *http.Response) ([]byte, string) {
	if resp == nil {
		return nil, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, imageDownloadLimit))
	if err != nil || len(body) == 0 {
		return nil, ""
	}
	return body, resp.Header.Get("Content-Type")
}

// ---------------------------------------------------------------------------
// helpers

func randReqID() int {
	return 10000 + rand.IntN(90000)
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
