package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// memKV 是 configStore 的单进程测试后端。
type memKV struct {
	values map[string][]byte
}

func newMemKV() *memKV { return &memKV{values: map[string][]byte{}} }

func (m *memKV) Load(key string) ([]byte, bool, error) {
	v, ok := m.values[key]
	return v, ok, nil
}

func (m *memKV) Save(key string, data []byte) error {
	m.values[key] = append([]byte(nil), data...)
	return nil
}

func (m *memKV) Delete(key string) error {
	delete(m.values, key)
	return nil
}

func TestConfigStoreMigratesOnceAndUsesDatabaseAfterwards(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	configPath := filepath.Join(dir, "config.json")
	omniPath := filepath.Join(dir, "omnigate.json")

	hash, err := panelauth.HashPassword("pw123456")
	if err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"listen":          ":17863",
		"api_key":         "sk-legacy",
		"data_dir":        dataDir,
		"omnigate_config": omniPath,
		"storage":         map[string]any{"driver": "postgres", "dsn": "postgres://test"},
		"panel_auth":      map[string]any{"session_hours": 12, "users": []map[string]any{{"username": "admin", "role": "admin", "password_hash": hash}}},
		"outbound":        map[string]any{"proxies": []any{}, "routes": map[string]any{}},
		"request_archive": true,
		"unknown_legacy":  "kept only in migration backup",
	}
	rawLegacy, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.WriteFile(configPath, rawLegacy, 0o600); err != nil {
		t.Fatal(err)
	}
	rawOmni := []byte(`{"default_provider":"","providers":[{"name":"test","type":"openai","base_url":"https://example.com/v1","api_key":"upstream-key"}]}`)
	if err := os.WriteFile(omniPath, rawOmni, 0o600); err != nil {
		t.Fatal(err)
	}

	boot, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	backend := newMemKV()
	store, err := loadConfigStore(backend, boot, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Main().APIKey; got != "sk-legacy" {
		t.Fatalf("api_key = %q", got)
	}
	if got := len(store.Main().PanelAuth.Users); got != 1 {
		t.Fatalf("panel users = %d, want 1", got)
	}
	if got := store.Omni().Providers; len(got) != 1 || got[0].Name != "test" {
		t.Fatalf("providers = %+v", got)
	}

	// 旧文件已被备份；config.json 只剩 bootstrap，omnigate.json 不再存在。
	var bootFile map[string]any
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &bootFile); err != nil {
		t.Fatal(err)
	}
	if _, ok := bootFile["api_key"]; ok {
		t.Fatal("bootstrap config still contains api_key")
	}
	st, _ := bootFile["storage"].(map[string]any)
	if st == nil || st["driver"] != "postgres" {
		t.Fatalf("bootstrap storage = %#v", bootFile["storage"])
	}
	if _, err := os.Stat(omniPath); !os.IsNotExist(err) {
		t.Fatalf("legacy omnigate.json still exists: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dataDir, "backups", "config-migration-*", "omnigate.json"))
	if len(matches) != 1 {
		t.Fatalf("migration backup = %v", matches)
	}

	// 修改旧文件不会影响后续启动：数据库行已存在，loadConfigStore 直接读库。
	if err := os.WriteFile(configPath, []byte(`{"storage":{"driver":"postgres","dsn":"postgres://test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	boot2, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store2, err := loadConfigStore(backend, boot2, configPath)
	if err != nil {
		t.Fatal(err)
	}
	// 生产启动在 ensurePanelUsers 之后注入权威账号视图。
	store2.SetUsers(store.Main().PanelAuth.Users)
	if got := store2.Main().APIKey; got != "sk-legacy" {
		t.Fatalf("api_key after reload = %q", got)
	}
	if got := store2.Main().Listen; got != ":17863" {
		t.Fatalf("listen after reload = %q", got)
	}

	// 保存主配置：存储行里不出现 DSN / 面板密码哈希；上一版进备份键。
	main := store2.Main()
	main.APIKey = "sk-new"
	// 模拟面板表单回传：账号段被清空/掩码，运行时应保留权威账号视图。
	main.PanelAuth.Users = nil
	if err := store2.SaveMain(main); err != nil {
		t.Fatal(err)
	}
	if got := len(store2.Users()); got != 1 {
		t.Fatalf("in-memory users after save = %d, want 1", got)
	}
	if got := len(store2.Main().PanelAuth.Users); got != 1 {
		t.Fatalf("runtime users after save = %d, want 1", got)
	}
	var bundle configBundle
	if err := json.Unmarshal(backend.values[configStoreKey], &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Revision != 2 {
		t.Fatalf("revision = %d, want 2", bundle.Revision)
	}
	var storedMain struct {
		Storage   StorageConfig `json:"storage"`
		PanelAuth struct {
			Users []panelauth.User `json:"users"`
		} `json:"panel_auth"`
	}
	if err := json.Unmarshal(bundle.Main, &storedMain); err != nil {
		t.Fatal(err)
	}
	if storedMain.Storage.DSN != "" {
		t.Fatalf("DSN persisted in config row: %q", storedMain.Storage.DSN)
	}
	if len(storedMain.PanelAuth.Users) != 0 {
		t.Fatalf("password hashes persisted in config row: %d users", len(storedMain.PanelAuth.Users))
	}
	if _, ok := backend.values[configStoreBackupKey]; !ok {
		t.Fatal("previous config backup was not written")
	}

	// 新的 bootstrap 读回数据库中的新密钥。
	store3, err := loadConfigStore(backend, boot2, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := store3.Main().APIKey; got != "sk-new" {
		t.Fatalf("api_key after save = %q", got)
	}

	// 当前行损坏时回退到上一版（备份键），而不是回落本地文件。
	backend.values[configStoreKey] = []byte("{broken")
	store4, err := loadConfigStore(backend, boot2, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := store4.Main().APIKey; got != "sk-legacy" {
		t.Fatalf("api_key from backup = %q", got)
	}
}

// 旧版部署常把 omnigate.json 放在 data_dir 下，而 config.json 里写的是相对
// "omnigate.json"（由 OMNIGATE_CONFIG 指向别处）。迁移必须回落到 data_dir。
func TestConfigStoreFindsLegacyOmniUnderDataDir(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	configPath := filepath.Join(dir, "config.json")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rawLegacy := []byte(`{"listen":":17863","data_dir":"` + filepath.ToSlash(dataDir) + `","omnigate_config":"omnigate.json","storage":{"driver":"postgres","dsn":"postgres://test"}}`)
	if err := os.WriteFile(configPath, rawLegacy, 0o600); err != nil {
		t.Fatal(err)
	}
	rawOmni := []byte(`{"default_provider":"p","providers":[{"name":"p","type":"openai","base_url":"https://example.com/v1","api_key":"k"}]}`)
	if err := os.WriteFile(filepath.Join(dataDir, "omnigate.json"), rawOmni, 0o600); err != nil {
		t.Fatal(err)
	}

	boot, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := loadConfigStore(newMemKV(), boot, configPath)
	if err != nil {
		t.Fatal(err)
	}
	providers := store.Omni().Providers
	if len(providers) != 1 || providers[0].Name != "p" {
		t.Fatalf("providers = %+v", providers)
	}
}
