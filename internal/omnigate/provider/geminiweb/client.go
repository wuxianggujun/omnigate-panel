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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
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
}

// NewClient builds a client with a direct transport; SetProxy / SetProxySelector
// re-route it.
func NewClient() *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
				ResponseHeaderTimeout: 180 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
}

// SetProxy routes this client's outbound requests through u (nil = direct).
func (c *Client) SetProxy(u *url.URL) {
	if u == nil {
		return
	}
	if tr, ok := c.http.Transport.(*http.Transport); ok {
		tr.Proxy = http.ProxyURL(u)
		tr.CloseIdleConnections()
	}
}

// SetProxySelector routes this client's outbound requests through the proxy pool
// selected per request by sel (nil = direct).
func (c *Client) SetProxySelector(sel outbound.Selector) {
	if c == nil || c.http == nil {
		return
	}
	base, ok := c.http.Transport.(*http.Transport)
	if !ok {
		return
	}
	c.http.Transport = outbound.WrapTransport(base, sel)
}

// setNavHeaders stamps a document navigation (the Google → Gemini handshake).
func (c *Client) setNavHeaders(req *http.Request) {
	req.Header.Set("User-Agent", provider.BrowserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", provider.AcceptLanguage)
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
}

// setXHRHeaders stamps a same-origin XHR (the batchexecute / StreamGenerate
// calls). accept overrides the default Accept when non-empty.
func (c *Client) setXHRHeaders(req *http.Request, accept string) {
	req.Header.Set("User-Agent", provider.BrowserUA)
	if accept == "" {
		accept = "*/*"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", provider.AcceptLanguage)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Origin", "https://gemini.google.com")
	req.Header.Set("Referer", "https://gemini.google.com/app")
}

// ---------------------------------------------------------------------------
// cookie jar

// cookieJar keeps an account's Google cookies and merges rotations the upstream
// returns on any response (__Secure-1PSIDTS rotates; __Secure-1PSID is stable).
type cookieJar struct {
	vals map[string]string
}

func parseCookies(raw string) *cookieJar {
	j := &cookieJar{vals: map[string]string{}}
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
		j.vals[k] = strings.TrimSpace(v)
	}
	return j
}

func (j *cookieJar) get(k string) string {
	if j == nil {
		return ""
	}
	return j.vals[k]
}

func (j *cookieJar) set(k, v string) {
	if j == nil || k == "" || v == "" {
		return
	}
	j.vals[k] = v
}

// header renders the jar as a Cookie header value (sorted for stability).
func (j *cookieJar) header() string {
	if j == nil || len(j.vals) == 0 {
		return ""
	}
	names := make([]string, 0, len(j.vals))
	for k := range j.vals {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, k+"="+j.vals[k])
	}
	return strings.Join(parts, "; ")
}

// updateFrom merges Set-Cookie values from a response.
func (j *cookieJar) updateFrom(resp *http.Response) {
	if j == nil || resp == nil {
		return
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "" {
			continue
		}
		j.set(ck.Name, ck.Value)
	}
}

// ---------------------------------------------------------------------------
// protocol operations

// acquire walks the Google → Gemini handshake, collecting rotated cookies and
// extracting the SNlM0e token and upload push id the generate endpoint needs.
func (c *Client) acquire(ctx context.Context, jar *cookieJar) (token, pushID string, err error) {
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
func (c *Client) getPage(ctx context.Context, jar *cookieJar, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	c.setNavHeaders(req)
	if h := jar.header(); h != "" {
		req.Header.Set("Cookie", h)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	jar.updateFrom(resp)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return "", &UpstreamError{Status: resp.StatusCode, Message: clip(string(raw))}
	}
	return string(raw), nil
}

// rotateCookies asks Google to rotate __Secure-1PSIDTS. This is ordinary
// credential maintenance (the same call the browser makes), invoked on demand
// when a session has gone stale — not a background keep-alive loop.
func (c *Client) rotateCookies(ctx context.Context, jar *cookieJar) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rotateCookiesURL,
		strings.NewReader(`[000,"-0000000000000000000"]`))
	if err != nil {
		return err
	}
	c.setXHRHeaders(req, "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://accounts.google.com")
	if h := jar.header(); h != "" {
		req.Header.Set("Cookie", h)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	jar.updateFrom(resp)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &UpstreamError{Status: resp.StatusCode, Message: "RotateCookies 失败"}
	}
	return nil
}

// status calls otAQ7b (GetUserStatus) to learn which concrete models the account
// currently has, keyed by family.
func (c *Client) status(ctx context.Context, jar *cookieJar, sessionToken string) (map[string]string, error) {
	raw, err := c.batchexecute(ctx, jar, sessionToken, "otAQ7b", "[]")
	if err != nil {
		return nil, err
	}
	return parseModelsFromStatus(raw), nil
}

// batchexecute performs one generic RPC, reusing the heartbeat URL params and
// headers template.
func (c *Client) batchexecute(ctx context.Context, jar *cookieJar, sessionToken, rpcID, payload string) (string, error) {
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
		batchexecuteURL+"?"+q.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	c.setXHRHeaders(req, "*/*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("x-goog-ext-525001261-jspb", "[1,null,null,null,null,null,null,null,[4]]")
	req.Header.Set("x-goog-ext-73010989-jspb", "[0]")
	req.Header.Set("X-Same-Domain", "1")
	if h := jar.header(); h != "" {
		req.Header.Set("Cookie", h)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	jar.updateFrom(resp)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return "", &UpstreamError{Status: resp.StatusCode, Message: clip(string(raw))}
	}
	return string(raw), nil
}

// generate posts one turn to StreamGenerate and returns the raw response so the
// caller can stream it.
func (c *Client) generate(ctx context.Context, jar *cookieJar, sessionToken, sessionUUID, prompt, model, convID string, thinking bool) (*http.Response, error) {
	if strings.TrimSpace(sessionToken) == "" {
		return nil, &UpstreamError{Status: 401, Message: "缺少会话 token"}
	}
	var encoded string
	extra := map[string]string{}
	if thinking {
		encoded = encodePayloadThinking(prompt, model, convID, nil, "", sessionUUID)
		for k, v := range buildModelHeaderThinking(model, sessionUUID) {
			extra[k] = v
		}
		extra["x-goog-ext-525005358-jspb"] = `["` + sessionUUID + `",1]`
	} else {
		encoded = encodePayload(prompt, model, convID, nil, "")
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
	if h := jar.header(); h != "" {
		req.Header.Set("Cookie", h)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	jar.updateFrom(resp)
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
func (c *Client) downloadImage(ctx context.Context, jar *cookieJar, rawURL string) ([]byte, string) {
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
func (c *Client) imageRequest(ctx context.Context, jar *cookieJar, u string, follow bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", provider.BrowserUA)
	req.Header.Set("Accept-Language", provider.AcceptLanguage)
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	req.Header.Set("Referer", imageReferer)
	if h := jar.header(); h != "" {
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
