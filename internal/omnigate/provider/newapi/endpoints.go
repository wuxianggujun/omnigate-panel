package newapi

import "github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"

// Endpoints is the relay's user-facing API surface.
//
// It is defined once in the plugin SPI (plugin.UserAPIEndpoints) so that the
// operator-facing config, the gateway's spec mapping and this adapter cannot
// drift apart; this file only gives it the adapter-local name the rest of the
// package already uses.
type Endpoints = plugin.UserAPIEndpoints

// Auth modes, re-exported for readability inside this package.
const (
	AuthBearer = plugin.UserAPIKeyAuthBearer
	AuthCookie = plugin.UserAPIKeyAuthCookie
)

// DefaultEndpoints is the surface of the relay this adapter was originally
// built for.
func DefaultEndpoints() Endpoints { return plugin.DefaultUserAPIEndpoints() }
