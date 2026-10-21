// configstore.go PostgreSQL 配置存储：面板主配置 + OmniGate 引擎配置作为
// 单个版本化 JSON 文档落库（panel_users_kv 的 panel_config 行）。
//
// 迁移后运行时不再读写 config.json 的配置段与 omnigate.json；config.json 只保留
// PostgreSQL DSN（bootstrap），因为“连数据库需要数据库地址”这件事无法由数据库
// 自己提供。其余 listen/data_dir/api_key/outbound/logging/panel_auth 参数、供应商、
// 路由、账号与网关密钥全部以 PostgreSQL 为唯一权威。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/kv"
	omniconfig "github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

const (
	// configStoreKey 是主配置 + 引擎配置的数据库行。单行写入保证一次迁移/保存
	// 不会出现“主配置已换、引擎配置还是旧的”半截状态。
	configStoreKey = "panel_config"
	// configStoreBackupKey 保存上一版完整配置。当前行损坏时启动回退到它。
	configStoreBackupKey = "panel_config_previous"
	// configStoreVersion 是 JSON 文档结构版本，不兼容旧行时直接报错。
	configStoreVersion = 2
	// configStoreRef 是面板里展示的“配置位置”，不再暴露本地文件路径。
	configStoreRef = "postgres:panel_config"
)

// configBundle 是 panel_config 行的 JSON 结构。
type configBundle struct {
	Version   int             `json:"version"`
	Revision  int64           `json:"revision"`
	UpdatedAt time.Time       `json:"updated_at"`
	Main      json.RawMessage `json:"main"`
	Omnigate  json.RawMessage `json:"omnigate"`
}

// configStore 持有进程内配置快照。写操作串行化；单引擎进程模型下 revision
// 只用于审计/排障，不做多实例乐观锁。
type configStore struct {
	mu        sync.Mutex
	backend   kv.Backend
	bootstrap *Config // 只取 Storage（DSN）以及首次迁移的兜底默认值
	bundle    configBundle
	main      *Config
	omni      *omniconfig.Config
	// users 是进程内的面板账号视图（权威副本在 panel_users 表）。主配置行不存
	// 密码哈希，面板回传的又是掩码，因此保存时不能用提交值覆盖它。
	users []panelauth.User
}

// loadConfigStore 读取 PostgreSQL 配置；数据库为空时把现有 JSON 一次性导入。
// 导入成功后会把 config.json 收缩为 bootstrap、把 omnigate.json 移到备份目录，
// 之后启动不再读取这两个文件的配置内容。
func loadConfigStore(backend kv.Backend, boot *Config, configPath string) (*configStore, error) {
	if backend == nil {
		return nil, errors.New("配置存储不可用：必须启用 PostgreSQL")
	}
	s := &configStore{
		backend:   backend,
		bootstrap: cloneMainConfig(boot),
	}
	raw, found, err := backend.Load(configStoreKey)
	if err != nil {
		return nil, fmt.Errorf("从 PostgreSQL 读取配置失败: %w", err)
	}
	if !found {
		if err := s.migrateLegacy(boot, configPath); err != nil {
			return nil, err
		}
		return s, nil
	}
	bundle, err := decodeConfigBundle(raw)
	if err != nil {
		log.Printf("WARN: [config] PostgreSQL 当前配置无法解析，尝试上一版本: %v", err)
		backup, ok, berr := backend.Load(configStoreBackupKey)
		if berr != nil {
			return nil, fmt.Errorf("读取配置备份失败: %w", berr)
		}
		if !ok {
			return nil, fmt.Errorf("PostgreSQL 配置损坏且没有备份: %w", err)
		}
		bundle, err = decodeConfigBundle(backup)
		if err != nil {
			return nil, fmt.Errorf("PostgreSQL 当前配置与备份均无法解析: %w", err)
		}
		log.Printf("WARN: [config] 已回退到 PostgreSQL 中的上一版配置")
	}
	s.bundle = bundle
	if err := s.applyBundle(); err != nil {
		return nil, err
	}
	return s, nil
}

// BootstrapStorage 返回启动引导 DSN（来自 config.json 或环境变量）。
func (s *configStore) BootstrapStorage() StorageConfig {
	if s == nil {
		return StorageConfig{}
	}
	return s.bootstrap.Storage
}

// Users 返回进程内面板账号视图的副本。
func (s *configStore) Users() []panelauth.User {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]panelauth.User(nil), s.users...)
}

// SetUsers 更新进程内的面板账号视图（权威副本仍在 panel_users 表）。
func (s *configStore) SetUsers(users []panelauth.User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users = append([]panelauth.User(nil), users...)
	if s.main != nil {
		s.main.PanelAuth.Users = s.users
	}
}

// Main 返回当前主配置。调用方只读；保存会替换整个快照。
func (s *configStore) Main() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.main
}

// Omni 返回引擎配置的深拷贝。引擎构建会修改内存中的 PanelKey/APIKeys，
// 不能把 store 里的指针直接交出去。
func (s *configStore) Omni() *omniconfig.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneOmniConfig(s.omni)
}

// SaveMain 持久化主配置。Storage 始终取 bootstrap，不落库；panel_auth.users
// 由 panel_users 表权威保存，配置行里不重复存密码哈希。
func (s *configStore) SaveMain(c *Config) error {
	if c == nil {
		return errors.New("保存空配置")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 面板账号不在主配置行里；即使表单回传了掩码哈希，也以进程内权威视图为准。
	c.PanelAuth.Users = append([]panelauth.User(nil), s.users...)
	stored, err := marshalStoredMain(c)
	if err != nil {
		return err
	}
	next := s.bundle
	next.Main = stored
	next.Revision = s.bundle.Revision + 1
	next.UpdatedAt = time.Now().UTC()
	next.Version = configStoreVersion
	if err := s.persistBundleLocked(next); err != nil {
		return err
	}
	// 运行时快照保留完整字段（含 bootstrap storage 与面板账号），便于面板展示。
	c.Storage = s.bootstrap.Storage
	c.OmnigateConfig = configStoreRef + "#omnigate"
	s.main = c
	s.bundle = next
	return nil
}

// SaveOmni 持久化引擎配置（供应商 / 账号 / 路由 / 网关密钥）。
func (s *configStore) SaveOmni(c *omniconfig.Config) error {
	if c == nil {
		return errors.New("保存空引擎配置")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("编码引擎配置失败: %w", err)
	}
	next := s.bundle
	next.Omnigate = stored
	next.Revision = s.bundle.Revision + 1
	next.UpdatedAt = time.Now().UTC()
	next.Version = configStoreVersion
	if err := s.persistBundleLocked(next); err != nil {
		return err
	}
	s.omni = cloneOmniConfig(c)
	s.bundle = next
	return nil
}

// persistBundleLocked 把当前 bundle 复制到备份键，再原子覆盖主键。
// 调用方必须持 s.mu。
func (s *configStore) persistBundleLocked(next configBundle) error {
	oldRaw, err := json.Marshal(s.bundle)
	if err != nil {
		return fmt.Errorf("编码旧配置失败: %w", err)
	}
	newRaw, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("编码新配置失败: %w", err)
	}
	// 备份失败不阻塞主写入：数据库整体故障时主写入也会失败；单次备份失败
	// 只意味着少一份历史版本，不能因此让面板无法保存。
	if s.bundle.Revision > 0 {
		if err := s.backend.Save(configStoreBackupKey, oldRaw); err != nil {
			log.Printf("WARN: [config] 写配置备份失败（继续保存当前版本）: %v", err)
		}
	}
	if err := s.backend.Save(configStoreKey, newRaw); err != nil {
		return fmt.Errorf("写入 PostgreSQL 配置失败: %w", err)
	}
	return nil
}

func (s *configStore) applyBundle() error {
	if s.bundle.Version != 0 && s.bundle.Version != configStoreVersion {
		return fmt.Errorf("不支持的配置版本 %d（当前只支持 %d）", s.bundle.Version, configStoreVersion)
	}
	main, err := parseStoredMain(s.bundle.Main, s.bootstrap)
	if err != nil {
		return fmt.Errorf("解析主配置失败: %w", err)
	}
	omni, err := parseStoredOmni(s.bundle.Omnigate)
	if err != nil {
		return fmt.Errorf("解析引擎配置失败: %w", err)
	}
	s.main = main
	s.omni = omni
	// 数据库行里没有密码哈希，users 由 main 在 ensurePanelUsers 之后注入。
	s.users = nil
	return nil
}

// migrateLegacy 把旧版 config.json + omnigate.json 写入 PostgreSQL，并在成功
// 落库后收缩/移除旧文件。只在 panel_config 行不存在时执行一次。
func (s *configStore) migrateLegacy(boot *Config, configPath string) error {
	main := cloneMainConfig(boot)
	if main.APIKey == "" {
		key, err := GenerateAPIKey()
		if err != nil {
			return fmt.Errorf("生成网关密钥失败: %w", err)
		}
		main.APIKey = key
	}
	omniPath := resolveLegacyOmniPath(boot.OmnigateConfig, boot.DataDir, configPath)
	omni, rawOmni, err := loadLegacyOmni(omniPath)
	if err != nil {
		return fmt.Errorf("读取旧引擎配置 %s 失败: %w", omniPath, err)
	}
	log.Printf("[config] 迁移旧引擎配置 %s（%d 字节）", omniPath, len(rawOmni))
	rawMain, rawMainErr := os.ReadFile(configPath)
	if rawMainErr != nil && !os.IsNotExist(rawMainErr) {
		return fmt.Errorf("读取旧主配置 %s 失败: %w", configPath, rawMainErr)
	}
	storedMain, err := marshalStoredMain(main)
	if err != nil {
		return err
	}
	storedOmni, err := json.Marshal(omni)
	if err != nil {
		return fmt.Errorf("编码引擎配置失败: %w", err)
	}
	bundle := configBundle{
		Version:   configStoreVersion,
		Revision:  1,
		UpdatedAt: time.Now().UTC(),
		Main:      storedMain,
		Omnigate:  storedOmni,
	}
	rawBundle, err := json.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("编码配置失败: %w", err)
	}
	if err := s.backend.Save(configStoreKey, rawBundle); err != nil {
		return fmt.Errorf("写入 PostgreSQL 配置失败: %w", err)
	}
	// 初始版本也写一份备份：第一次面板保存前当前行若损坏，启动仍可回退。
	if err := s.backend.Save(configStoreBackupKey, rawBundle); err != nil {
		log.Printf("WARN: [config] 写初始配置备份失败（继续启动）: %v", err)
	}
	s.bundle = bundle
	s.main = main
	s.omni = omni
	s.users = append([]panelauth.User(nil), main.PanelAuth.Users...)

	s.retireLegacyFiles(configPath, omniPath, rawMain, rawOmni)
	log.Printf("[config] 已把 config.json / omnigate.json 迁移到 PostgreSQL（%s）；"+
		"后续只读写数据库配置", configStoreRef)
	return nil
}

// retireLegacyFiles 备份旧文件，然后把 config.json 收缩为 DSN bootstrap，
// 并把 omnigate.json 移到备份目录（不再被运行时读取）。
func (s *configStore) retireLegacyFiles(configPath, omniPath string, rawMain, rawOmni []byte) {
	backupDir := filepath.Join(s.bootstrap.DataDir, "backups",
		"config-migration-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		log.Printf("WARN: [config] 创建迁移备份目录失败，保留旧配置文件: %v", err)
		return
	}
	if len(rawMain) > 0 {
		if err := os.WriteFile(filepath.Join(backupDir, "config.json"), rawMain, 0o600); err != nil {
			log.Printf("WARN: [config] 备份 config.json 失败，暂不收缩旧文件: %v", err)
			return
		}
	}
	if len(rawOmni) > 0 {
		if err := os.WriteFile(filepath.Join(backupDir, "omnigate.json"), rawOmni, 0o600); err != nil {
			log.Printf("WARN: [config] 备份 omnigate.json 失败，暂不移除旧文件: %v", err)
			return
		}
	}
	if configPath != "" && len(rawMain) > 0 {
		bootstrap := struct {
			Storage StorageConfig `json:"storage"`
		}{Storage: s.bootstrap.Storage}
		out, err := json.MarshalIndent(bootstrap, "", "  ")
		if err == nil {
			out = append(out, '\n')
			if err := writeConfigFile(configPath, out, 0o600); err != nil {
				log.Printf("WARN: [config] 收缩 config.json 失败（数据库配置已生效）: %v", err)
			} else {
				log.Printf("[config] config.json 已收缩为 PostgreSQL bootstrap；旧文件备份在 %s", backupDir)
			}
		}
	}
	if len(rawOmni) > 0 && omniPath != "" {
		if err := os.Remove(omniPath); err != nil && !os.IsNotExist(err) {
			log.Printf("WARN: [config] 移除旧引擎配置 %s 失败（已忽略）: %v", omniPath, err)
		}
	}
}

// resolveLegacyOmniPath 兼容旧部署：omnigate_config 可能是相对路径、也可能被
// OMNIGATE_CONFIG 指到 data 目录。迁移时按「配置值 → data_dir/omnigate.json →
// config.json 同目录」依次探测；都不存在时仍返回首个候选，由调用方生成空引擎配置。
func resolveLegacyOmniPath(configured, dataDir, configPath string) string {
	var candidates []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, old := range candidates {
			if old == p {
				return
			}
		}
		candidates = append(candidates, p)
	}
	add(configured)
	if dataDir != "" {
		add(filepath.Join(dataDir, "omnigate.json"))
	}
	if configPath != "" {
		add(filepath.Join(filepath.Dir(configPath), "omnigate.json"))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return "omnigate.json"
}

func loadLegacyOmni(path string) (*omniconfig.Config, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c := &omniconfig.Config{}
			if err := c.Normalize(); err != nil {
				return nil, nil, err
			}
			return c, nil, nil
		}
		return nil, nil, err
	}
	c, err := parseOmniConfig(raw)
	if err != nil {
		return nil, raw, err
	}
	return c, raw, nil
}

func parseStoredMain(raw []byte, boot *Config) (*Config, error) {
	c := Default()
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, err
		}
	}
	if c.Listen == "" {
		c.Listen = boot.Listen
	}
	if c.DataDir == "" {
		c.DataDir = boot.DataDir
	}
	// DSN 只存在于 bootstrap；配置行里不重复存数据库密码。
	c.Storage = boot.Storage
	c.OmnigateConfig = configStoreRef + "#omnigate"
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func parseStoredOmni(raw []byte) (*omniconfig.Config, error) {
	if len(raw) == 0 {
		c := &omniconfig.Config{}
		if err := c.Normalize(); err != nil {
			return nil, err
		}
		return c, nil
	}
	return parseOmniConfig(raw)
}

func parseOmniConfig(raw []byte) (*omniconfig.Config, error) {
	var c omniconfig.Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

func decodeConfigBundle(raw []byte) (configBundle, error) {
	var b configBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	if b.Version != 0 && b.Version != configStoreVersion {
		return b, fmt.Errorf("不支持的配置版本 %d", b.Version)
	}
	if len(b.Main) == 0 {
		return b, errors.New("配置缺少 main")
	}
	return b, nil
}

func marshalStoredMain(c *Config) (json.RawMessage, error) {
	cp := *c
	cp.Storage = StorageConfig{}
	cp.OmnigateConfig = ""
	cp.PanelAuth.Users = nil
	cp.ServerReadTimeoutDur = 0
	raw, err := json.Marshal(&cp)
	if err != nil {
		return nil, fmt.Errorf("编码主配置失败: %w", err)
	}
	return raw, nil
}

func cloneMainConfig(c *Config) *Config {
	if c == nil {
		return nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		cp := *c
		return &cp
	}
	var out Config
	if err := json.Unmarshal(raw, &out); err != nil {
		cp := *c
		return &cp
	}
	out.ServerReadTimeoutDur = c.ServerReadTimeoutDur
	return &out
}

func cloneOmniConfig(c *omniconfig.Config) *omniconfig.Config {
	if c == nil {
		return nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		cp := *c
		return &cp
	}
	var out omniconfig.Config
	if err := json.Unmarshal(raw, &out); err != nil {
		cp := *c
		return &cp
	}
	if err := out.Normalize(); err != nil {
		cp := *c
		return &cp
	}
	return &out
}
