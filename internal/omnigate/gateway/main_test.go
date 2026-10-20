package gateway

import (
	"os"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin/catalog"
)

// TestMain registers the built-in provider catalog once for the whole package.
//
// Production always does this inside Gateway.New (see gateway.go), and the
// gateway now answers protocol-level questions (tool emulation, wire format,
// reasoning_effort, account mode, …) from the plugin descriptors rather than by
// switching on the type string. Tests that build a Gateway literal — bypassing
// New — would otherwise run against an empty registry and see every type degrade
// to zero traits, which is not the state the code under test ever faces in
// production. Registering here keeps the test gateways faithful to that.
func TestMain(m *testing.M) {
	catalog.Register()
	os.Exit(m.Run())
}
