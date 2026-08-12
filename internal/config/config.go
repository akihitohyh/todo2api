package config

import (
	"bufio"
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	mu           sync.Mutex
	sourcePath   string
	keyFilePaths []string
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
	if err := cfg.loadKeyFiles(filepath.Dir(absPath)); err != nil {
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
		path = filepath.Clean(path)
		c.keyFilePaths = append(c.keyFilePaths, path)
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

// RemovePoolKey deletes an account key from every configured source and from
// the loaded configuration. Rewrites use fsync plus rename so a crash cannot
// leave a partially written credential file.
func (c *Config) RemovePoolKey(apiKey string) error {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	type rewrite struct {
		path string
		data []byte
		mode os.FileMode
	}
	var rewrites []rewrite

	if c.sourcePath != "" {
		data, err := os.ReadFile(c.sourcePath)
		if err != nil {
			return fmt.Errorf("read config for key removal: %w", err)
		}
		updated, changed, err := removeInlinePoolKey(data, apiKey)
		if err != nil {
			return fmt.Errorf("remove key from config: %w", err)
		}
		if changed {
			info, err := os.Stat(c.sourcePath)
			if err != nil {
				return fmt.Errorf("stat config for key removal: %w", err)
			}
			rewrites = append(rewrites, rewrite{path: c.sourcePath, data: updated, mode: info.Mode()})
		}
	}

	for _, path := range c.keyFilePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read pool key file %s for key removal: %w", path, err)
		}
		updated, changed := removeKeyFileEntry(data, apiKey)
		if !changed {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat pool key file %s for key removal: %w", path, err)
		}
		rewrites = append(rewrites, rewrite{path: path, data: updated, mode: info.Mode()})
	}

	for _, rewrite := range rewrites {
		if err := atomicWriteFile(rewrite.path, rewrite.data, rewrite.mode); err != nil {
			return err
		}
	}

	keys := c.Pool.Keys[:0]
	for _, key := range c.Pool.Keys {
		if strings.TrimSpace(key.APIKey) != apiKey {
			keys = append(keys, key)
		}
	}
	c.Pool.Keys = keys
	return nil
}

func removeInlinePoolKey(data []byte, apiKey string) ([]byte, bool, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, false, err
	}
	if len(document.Content) == 0 {
		return data, false, nil
	}

	root := document.Content[0]
	pool := mappingValue(root, "pool")
	keys := mappingValue(pool, "keys")
	if keys == nil || keys.Kind != yaml.SequenceNode {
		return data, false, nil
	}

	filtered := keys.Content[:0]
	changed := false
	for _, item := range keys.Content {
		value := mappingValue(item, "api_key")
		if value != nil && strings.TrimSpace(os.ExpandEnv(value.Value)) == apiKey {
			changed = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !changed {
		return data, false, nil
	}
	keys.Content = filtered

	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, false, err
	}
	if err := encoder.Close(); err != nil {
		return nil, false, err
	}
	return output.Bytes(), true, nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func removeKeyFileEntry(data []byte, apiKey string) ([]byte, bool) {
	lines := bytes.SplitAfter(data, []byte("\n"))
	kept := lines[:0]
	changed := false
	for _, line := range lines {
		value := strings.TrimSpace(strings.TrimSuffix(string(line), "\n"))
		if value == apiKey {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	return bytes.Join(kept, nil), changed
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tempPath := temp.Name()
	defer func() {
		temp.Close()
		if err != nil {
			os.Remove(tempPath)
		}
	}()

	if err = temp.Chmod(mode.Perm()); err != nil {
		return fmt.Errorf("set mode on temporary file for %s: %w", path, err)
	}
	if _, err = temp.Write(data); err != nil {
		return fmt.Errorf("write temporary file for %s: %w", path, err)
	}
	if err = temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file for %s: %w", path, err)
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("close temporary file for %s: %w", path, err)
	}
	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}

	directory, openErr := os.Open(dir)
	if openErr != nil {
		return fmt.Errorf("open directory for %s: %w", path, openErr)
	}
	defer directory.Close()
	if err = directory.Sync(); err != nil {
		return fmt.Errorf("sync directory for %s: %w", path, err)
	}
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
