package config

import (
	"bufio"
	"fmt"
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
)

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	Upstream     UpstreamConfig     `yaml:"upstream"`
	Pool         PoolConfig         `yaml:"pool"`
	Models       ModelsConfig       `yaml:"models"`
	Edge         EdgeConfig         `yaml:"edge"`
	ToolProtocol ToolProtocolConfig `yaml:"tool_protocol"`
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
	APIKey    string `yaml:"api_key"`
	ProjectID string `yaml:"project_id"`
	AgentID   string `yaml:"agent_id"`
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
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(data))), &cfg); err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	if err := cfg.loadKeyFiles(filepath.Dir(path)); err != nil {
		return nil, err
	}
	cfg.setDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) loadKeyFiles(configDir string) error {
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
			return fmt.Errorf("pool.key_files must not contain an empty path")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(configDir, path)
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open pool key file %s: %w", path, err)
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
			return fmt.Errorf("read pool key file %s at line %d: %w", path, line, scanErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close pool key file %s: %w", path, closeErr)
		}
	}
	c.Pool.Keys = keys
	return nil
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
	if len(c.Pool.Keys) == 0 {
		return fmt.Errorf("pool.keys must contain at least one account")
	}
	for i, key := range c.Pool.Keys {
		if strings.TrimSpace(key.APIKey) == "" {
			return fmt.Errorf("pool.keys[%d].api_key must not be empty", i)
		}
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
