// Command gen-panel-types writes the panel's fallback provider metadata from the
// plugin catalog. Run it via `go generate ./internal/panel`; the panel package
// has a test that fails when the committed file is stale.
package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"

	"github.com/wuxianggujun/omnigate-panel/internal/panel"
)

func main() {
	root, err := moduleRoot()
	if err != nil {
		log.Fatal(err)
	}
	body, err := panel.BuiltinTypesJS()
	if err != nil {
		log.Fatal(err)
	}
	out := filepath.Join(root, "internal", "panel", "web", "js", "core", "builtin-types.generated.js")
	if err := os.WriteFile(out, body, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (%d bytes)", out, len(body))
}

// moduleRoot 从当前工作目录向上找 go.mod。这样无论从仓库根运行
// (`go run ./cmd/gen-panel-types`) 还是由 `go generate ./internal/panel`
// 在包目录里触发，写出的路径都一致。
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found")
		}
		dir = parent
	}
}
