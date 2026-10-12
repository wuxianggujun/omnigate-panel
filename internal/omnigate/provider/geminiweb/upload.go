package geminiweb

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// contentPushURL is where Gemini Web accepts attachment uploads. The response
// body is the file id, which is then injected into the StreamGenerate payload.
const contentPushURL = "https://content-push.googleapis.com/upload"

const (
	// maxRemoteAttachment bounds a caller-supplied remote attachment, both as a
	// resource limit and as part of the SSRF guard.
	maxRemoteAttachment = 20 << 20 // bytes
	// maxAttachmentRedirects bounds the hand-followed redirect chain.
	maxAttachmentRedirects = 5
)

// uploadAttachment uploads one attachment and returns the file reference Gemini
// expects in the payload (ok=false when it could not be uploaded).
//
// The header set is deliberately minimal. Google sniffs the body as its resumable
// upload protocol the moment any X-Goog-Upload-* header is present, and answers
// "Multipart body does not contain 2 or 3 parts" — the multipart form only works
// with the plain Origin/Referer/X-Tenant-Id/Push-ID combination.
func (c *Client) uploadAttachment(ctx context.Context, jar *cookieJar, pushID string, att openai.Attachment) (fileRef, bool) {
	data, filename, mime, ok := c.attachmentBytes(ctx, att)
	if !ok {
		return fileRef{}, false
	}
	filename = safeFilename(filename)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="` + filename + `"`},
		"Content-Type":        {mime},
	})
	if err != nil {
		return fileRef{}, false
	}
	if _, err := part.Write(data); err != nil {
		return fileRef{}, false
	}
	if err := mw.Close(); err != nil {
		return fileRef{}, false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.uploadEndpoint(), &body)
	if err != nil {
		return fileRef{}, false
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "https://gemini.google.com")
	req.Header.Set("Referer", "https://gemini.google.com/")
	req.Header.Set("X-Tenant-Id", "bard-storage")
	if pushID == "" {
		pushID = defaultPushID
	}
	req.Header.Set("Push-ID", pushID)
	req.Header.Set("User-Agent", provider.BrowserUA)
	req.Header.Set("Accept-Language", provider.AcceptLanguage)
	if h := jar.header(); h != "" {
		req.Header.Set("Cookie", h)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fileRef{}, false
	}
	defer resp.Body.Close()
	jar.updateFrom(resp)
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return fileRef{}, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return fileRef{}, false
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return fileRef{}, false
	}
	return fileRef{ID: id, Name: filename}, true
}

// attachmentBytes resolves an attachment to the bytes to upload, its display name
// and its mime type. An inline attachment is already decoded; a URL is fetched
// here, guarded.
func (c *Client) attachmentBytes(ctx context.Context, att openai.Attachment) ([]byte, string, string, bool) {
	filename := att.Filename
	if filename == "" {
		filename = "upload.bin"
	}
	mime := att.MIME
	if mime == "" {
		mime = "application/octet-stream"
	}
	if len(att.Data) > 0 {
		return att.Data, filename, mime, true
	}
	if att.URL == "" {
		return nil, "", "", false
	}
	data, mime, ok := c.fetchRemoteAttachment(ctx, att.URL, mime)
	if !ok {
		return nil, "", "", false
	}
	return data, filename, mime, true
}

// fetchRemoteAttachment downloads an attachment that lives at a URL, following
// redirects by hand so every hop is re-validated. A caller-supplied URL that
// redirects to a loopback or cloud-metadata address would otherwise reach it
// through this gateway's network position.
func (c *Client) fetchRemoteAttachment(ctx context.Context, rawURL, fallbackMime string) ([]byte, string, bool) {
	current := rawURL
	for hop := 0; hop <= maxAttachmentRedirects; hop++ {
		if !safeRemoteURL(current) {
			return nil, "", false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return nil, "", false
		}
		req.Header.Set("User-Agent", provider.BrowserUA)
		req.Header.Set("Accept-Language", provider.AcceptLanguage)

		// A private copy so the no-redirect rule applies to this request only; the
		// Transport (and therefore the proxy configuration) is shared.
		noRedirect := *c.http
		noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			return nil, "", false
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if loc == "" {
				return nil, "", false
			}
			current = resolveRedirect(current, loc)
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			return nil, "", false
		}
		// Check the declared length first, so an oversized body is rejected before
		// it is buffered; the actual byte count is checked after, in case the
		// header lied.
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n > maxRemoteAttachment {
				return nil, "", false
			}
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteAttachment+1))
		if err != nil || len(data) == 0 || len(data) > maxRemoteAttachment {
			return nil, "", false
		}
		mime := fallbackMime
		if ct := resp.Header.Get("Content-Type"); ct != "" && (mime == "" || mime == "application/octet-stream") {
			mime = strings.TrimSpace(strings.Split(ct, ";")[0])
		}
		return data, mime, true
	}
	return nil, "", false
}

// safeRemoteURL reports whether a caller-supplied URL may be fetched. The name is
// resolved and every address it resolves to must be publicly routable, which
// keeps loopback, private, link-local (cloud metadata) and multicast targets out
// of reach.
//
// Resolution here and the connection later are two separate lookups, so a name
// that answers differently the second time (DNS rebinding) is not fully covered.
// The cost of closing that gap is a custom dialer pinned to the vetted address;
// this guard is the same one the upstream ships.
func safeRemoteURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	// url.Parse already rejects a malformed port, so there is nothing to re-check
	// here.
	// A literal address needs no resolution, and must be vetted directly.
	if ip := net.ParseIP(host); ip != nil {
		return !unsafeIP(ip)
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if unsafeIP(ip) {
			return false
		}
	}
	return true
}

// unsafeIP reports whether an address is one a caller must not be able to reach
// through us.
func unsafeIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}
	// Go's standard library has no equivalent of Python's is_reserved, so the
	// non-routable ranges that are still "global unicast" are listed by hand.
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 100 && v4[1]&0xc0 == 64: // 100.64.0.0/10 CGNAT
			return true
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
			return true
		case v4[0] >= 240: // 240.0.0.0/4
			return true
		}
	}
	return false
}

// resolveRedirect turns a Location header into an absolute URL relative to the
// URL that produced it.
func resolveRedirect(base, loc string) string {
	b, err := url.Parse(base)
	if err != nil {
		return loc
	}
	l, err := url.Parse(strings.TrimSpace(loc))
	if err != nil {
		return loc
	}
	return b.ResolveReference(l).String()
}

// safeFilename strips anything that would break out of the multipart
// Content-Disposition header.
func safeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		return "upload.bin"
	}
	return name
}
