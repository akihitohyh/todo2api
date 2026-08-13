package config

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultAddr        = ":8080"
	defaultBaseURL     = "https://api.todofor.ai/api/v1"
	defaultPollTimeout = 5 * time.Minute
	defaultStoragePath = "data/todo2api.db"
	defaultSessionTTL  = 12 * time.Hour
)

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	Upstream     UpstreamConfig     `yaml:"upstream"`
	Pool         PoolConfig         `yaml:"pool"`
	Storage      StorageConfig      `yaml:"storage"`
	Web          WebConfig          `yaml:"web"`
	Models       ModelsConfig       `yaml:"models"`
	Edge         EdgeConfig         `yaml:"edge"`
	ToolProtocol ToolProtocolConfig `yaml:"tool_protocol"`

	sourcePath string
}

type ServerConfig struct {
	Addr         string   `yaml:"addr"`
	ClientTokens []string `yaml:"client_tokens"`
}

type UpstreamConfig struct {
	BaseURL     string        `yaml:"base_url"`
	PollTimeout time.Duration `yaml:"poll_timeout"`
}

type PoolConfig struct {
	Strategy string       `yaml:"strategy"`
	Keys     []AccountKey `yaml:"keys"`
	KeyFiles []string     `yaml:"key_files"`
}

type AccountKey struct {
	ID        int64  `yaml:"-"`
	APIKey    string `yaml:"api_key"`
	ProjectID string `yaml:"project_id"`
	AgentID   string `yaml:"agent_id"`
	Enabled   bool   `yaml:"-"`
}

type StorageConfig struct {
	Path      string `yaml:"path"`
	MasterKey string `yaml:"master_key"`
}

type WebConfig struct {
	AdminUsername  string        `yaml:"admin_username"`
	AdminPassword  string        `yaml:"admin_password"`
	SessionTTL     time.Duration `yaml:"session_ttl"`
	SecureCookie   bool          `yaml:"secure_cookie"`
	TrustedProxies []string      `yaml:"trusted_proxies"`
}

type ModelsConfig struct {
	Default string            `yaml:"default"`
	Aliases map[string]string `yaml:"aliases"`
}

func (m ModelsConfig) Resolve(model string) string {
	if model == "" {
		return m.Default
	}
	if resolved, ok := m.Aliases[model]; ok {
		return resolved
	}
	return model
}

func validUpstreamModelName(model string) bool {
	provider, modelID, ok := strings.Cut(model, ":")
	if !ok || provider == "" || modelID == "" {
		return false
	}
	author, name, ok := strings.Cut(modelID, "/")
	return ok && author != "" && name != ""
}

type EdgeConfig struct {
	Enabled    bool     `yaml:"enabled"`
	EdgeID     string   `yaml:"edge_id"`
	DeviceID   string   `yaml:"device_id"` // Backward-compatible alias for edge_id.
	AllowTools []string `yaml:"allow_tools"`
}

func (e EdgeConfig) ID() string {
	if e.EdgeID != "" {
		return e.EdgeID
	}
	return e.DeviceID
}

type ToolProtocolConfig struct {
	DenyUpstreamTools []string `yaml:"deny_upstream_tools"`
}

func Load(path string) (*Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}

	cfg := Config{sourcePath: absPath}
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(data))), &cfg); err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	cfg.setDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LegacyPoolKeys reads the retired YAML/file storage format. It is called only
// by the SQLite store before it writes the one-time migration marker.
func (c *Config) LegacyPoolKeys() ([]AccountKey, error) {
	configDir := filepath.Dir(c.sourcePath)
	seen := make(map[string]struct{}, len(c.Pool.Keys))
	keys := make([]AccountKey, 0, len(c.Pool.Keys))
	appendKey := func(key AccountKey) {
		key.APIKey = strings.TrimSpace(key.APIKey)
		if key.APIKey == "" {
			return
		}
		if _, exists := seen[key.APIKey]; exists {
			return
		}
		seen[key.APIKey] = struct{}{}
		keys = append(keys, key)
	}
	for _, key := range c.Pool.Keys {
		appendKey(key)
	}

	for _, configuredPath := range c.Pool.KeyFiles {
		path := os.ExpandEnv(strings.TrimSpace(configuredPath))
		if path == "" {
			return nil, fmt.Errorf("pool.key_files must not contain an empty path")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(configDir, path)
		}
		path = filepath.Clean(path)
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open pool key file %s: %w", path, err)
		}
		scanner := bufio.NewScanner(file)
		line := 0
		for scanner.Scan() {
			line++
			value := strings.TrimSpace(scanner.Text())
			if value == "" || strings.HasPrefix(value, "#") {
				continue
			}
			appendKey(AccountKey{APIKey: value})
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("read pool key file %s at line %d: %w", path, line, scanErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close pool key file %s: %w", path, closeErr)
		}
	}
	return keys, nil
}

func (c *Config) setDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = defaultAddr
	}
	if c.Upstream.BaseURL == "" {
		c.Upstream.BaseURL = defaultBaseURL
	}
	c.Upstream.BaseURL = strings.TrimRight(c.Upstream.BaseURL, "/")
	if c.Upstream.PollTimeout == 0 {
		c.Upstream.PollTimeout = defaultPollTimeout
	}
	if c.Pool.Strategy == "" {
		c.Pool.Strategy = "round_robin"
	}
	if c.Storage.Path == "" {
		c.Storage.Path = defaultStoragePath
	}
	if !filepath.IsAbs(c.Storage.Path) {
		c.Storage.Path = filepath.Join(filepath.Dir(c.sourcePath), c.Storage.Path)
	}
	c.Storage.Path = filepath.Clean(c.Storage.Path)
	if c.Web.SessionTTL == 0 {
		c.Web.SessionTTL = defaultSessionTTL
	}
	if c.Models.Aliases == nil {
		c.Models.Aliases = map[string]string{}
	}
	if c.ToolProtocol.DenyUpstreamTools == nil {
		c.ToolProtocol.DenyUpstreamTools = []string{"device:*", "cloud:*"}
	}
}

func (c *Config) Validate() error {
	u, err := url.Parse(c.Upstream.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("upstream.base_url must be an absolute HTTP(S) URL")
	}
	if c.Upstream.PollTimeout <= 0 {
		return fmt.Errorf("upstream.poll_timeout must be positive")
	}
	if c.Pool.Strategy != "round_robin" && c.Pool.Strategy != "least_busy" {
		return fmt.Errorf("pool.strategy must be round_robin or least_busy")
	}
	if _, err := c.MasterKey(); err != nil {
		return err
	}
	clientTokens := make(map[string]struct{}, len(c.Server.ClientTokens))
	for i, token := range c.Server.ClientTokens {
		token = strings.TrimSpace(token)
		if token == "" {
			return fmt.Errorf("server.client_tokens[%d] must not be empty", i)
		}
		if _, exists := clientTokens[token]; exists {
			return fmt.Errorf("server.client_tokens[%d] duplicates an earlier token", i)
		}
		clientTokens[token] = struct{}{}
		c.Server.ClientTokens[i] = token
	}
	for i, key := range c.Pool.Keys {
		if strings.TrimSpace(key.APIKey) == "" {
			return fmt.Errorf("pool.keys[%d].api_key must not be empty", i)
		}
	}
	if strings.TrimSpace(c.Web.AdminUsername) == "" || c.Web.AdminPassword == "" {
		return fmt.Errorf("web.admin_username and web.admin_password must be configured")
	}
	if c.Web.SessionTTL <= 0 {
		return fmt.Errorf("web.session_ttl must be positive")
	}
	for i, cidr := range c.Web.TrustedProxies {
		cidr = strings.TrimSpace(cidr)
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("web.trusted_proxies[%d] must be a valid CIDR", i)
		}
		c.Web.TrustedProxies[i] = cidr
	}
	if c.Models.Default == "" && len(c.Models.Aliases) == 0 {
		return fmt.Errorf("models.default or models.aliases must be configured")
	}
	if c.Models.Default != "" && !validUpstreamModelName(c.Models.Default) {
		return fmt.Errorf("models.default must use provider:author/model_id format")
	}
	for alias, model := range c.Models.Aliases {
		if !validUpstreamModelName(model) {
			return fmt.Errorf("models.aliases[%q] must use provider:author/model_id format", alias)
		}
	}
	return nil
}

// MasterKey decodes the mandatory AES-256 master key from the loaded config.
func (c *Config) MasterKey() ([]byte, error) {
	raw := strings.TrimSpace(c.Storage.MasterKey)
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("storage.master_key must be base64-encoded 32 bytes")
	}
	return key, nil
}
