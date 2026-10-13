package panel

import (
	"strings"
	"testing"
)

// storage.dsn 里带数据库密码：面板回显配置时必须脱敏，且掩码能按位还原。
func TestMaskStorageDSN(t *testing.T) {
	dsn := "postgres://omnigate:s3cr3t@1Panel-postgresql-5NdW:5432/omnigate_panel?sslmode=disable"
	cfg := map[string]any{
		"storage": map[string]any{"driver": "postgres", "dsn": dsn},
	}

	out := maskSecrets(cfg).(map[string]any)
	st := out["storage"].(map[string]any)
	if st["driver"] != "postgres" {
		t.Fatalf("driver 不该被改: %v", st["driver"])
	}
	got, _ := st["dsn"].(string)
	if got == dsn || !isMasked(got) {
		t.Fatalf("dsn 应脱敏，得到 %q", got)
	}
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("脱敏后仍残留数据库密码: %q", got)
	}

	back := restoreSecrets(out, cfg).(map[string]any)["storage"].(map[string]any)
	if back["dsn"] != dsn {
		t.Fatalf("dsn 应能还原成原值，得到 %q", back["dsn"])
	}
}
