package plugin

import "strings"

// UserAPIEndpoints describes a New API / One API relay's *user-facing* API
// surface.
//
// New API is a single product with many deployments, and the deployments differ
// in exactly four ways:
//
//	check-in path      /api/user/checkin   vs  /api/user/sign_in
//	auth mechanism     Bearer <token>      vs  Cookie: session=…
//	user identity      implicit in token   vs  New-Api-User: <numeric id>
//	sign-in page       /sign-in            vs  /login (or a custom path)
//
// AnyRouter and AgentRouter are both New API deployments, so the chat path, the
// envelope shape and the token lifecycle all apply unchanged — only those four
// facts differ. They are therefore configuration on ProviderConfig, carried here
// through the SPI, instead of being hardcoded in one adapter.
//
// This lives in plugin (not in provider/newapi) so the SPI can describe the
// surface without importing a concrete provider package.
type UserAPIEndpoints struct {
	// CheckinPath is the daily check-in endpoint (default /api/user/checkin).
	CheckinPath string
	// SelfPath returns the account profile + quota (default /api/user/self).
	SelfPath string
	// RefreshPath rotates the access token (default /api/user/auth/refresh).
	RefreshPath string
	// ModelsPath lists chat models (default /v1/models).
	ModelsPath string
	// ChatPath is the chat completion endpoint (default /v1/chat/completions).
	ChatPath string
	// SigninPath is the browser login page opened by the wizard (default
	// /sign-in).
	SigninPath string
	// UserHeader, when set, is sent as "<UserHeader>: <user id>" on every user
	// API request. AnyRouter/AgentRouter require "New-Api-User".
	UserHeader string
	// AuthMode is how the user API is authenticated: "bearer" (default) sends
	// Authorization: Bearer <access token>; "cookie" sends the stored session
	// cookie and relies on UserHeader for identity.
	AuthMode string
}

// User-API auth modes.
const (
	UserAPIKeyAuthBearer = "bearer"
	UserAPIKeyAuthCookie = "cookie"
)

// DefaultUserAPIEndpoints is the surface of the relay the newapi adapter was
// originally built for. Every zero field falls back to it, so existing configs
// are byte-for-byte unaffected.
func DefaultUserAPIEndpoints() UserAPIEndpoints {
	return UserAPIEndpoints{
		CheckinPath: "/api/user/checkin",
		SelfPath:    "/api/user/self",
		RefreshPath: "/api/user/auth/refresh",
		ModelsPath:  "/v1/models",
		ChatPath:    "/v1/chat/completions",
		SigninPath:  "/sign-in",
		AuthMode:    UserAPIKeyAuthBearer,
	}
}

// Normalized fills every empty field with its default, canonicalises AuthMode and
// forces paths to be absolute.
func (e UserAPIEndpoints) Normalized() UserAPIEndpoints {
	def := DefaultUserAPIEndpoints()
	out := e
	if strings.TrimSpace(out.CheckinPath) == "" {
		out.CheckinPath = def.CheckinPath
	}
	if strings.TrimSpace(out.SelfPath) == "" {
		out.SelfPath = def.SelfPath
	}
	if strings.TrimSpace(out.RefreshPath) == "" {
		out.RefreshPath = def.RefreshPath
	}
	if strings.TrimSpace(out.ModelsPath) == "" {
		out.ModelsPath = def.ModelsPath
	}
	if strings.TrimSpace(out.ChatPath) == "" {
		out.ChatPath = def.ChatPath
	}
	if strings.TrimSpace(out.SigninPath) == "" {
		out.SigninPath = def.SigninPath
	}
	out.UserHeader = strings.TrimSpace(out.UserHeader)
	if strings.EqualFold(strings.TrimSpace(out.AuthMode), UserAPIKeyAuthCookie) {
		out.AuthMode = UserAPIKeyAuthCookie
	} else {
		out.AuthMode = UserAPIKeyAuthBearer
	}
	// Paths are joined onto the origin; a leading slash keeps net/url honest.
	for _, p := range []*string{&out.CheckinPath, &out.SelfPath, &out.RefreshPath,
		&out.ModelsPath, &out.ChatPath, &out.SigninPath} {
		if !strings.HasPrefix(*p, "/") {
			*p = "/" + *p
		}
	}
	return out
}

// UsesCookieAuth reports whether the user API authenticates by session cookie
// (AnyRouter/AgentRouter) rather than a bearer access token.
func (e UserAPIEndpoints) UsesCookieAuth() bool {
	return e.Normalized().AuthMode == UserAPIKeyAuthCookie
}
