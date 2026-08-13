package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsAndEnvironmentExpansion(t *testing.T) {
	t.Setenv("TEST_TODOFOR_KEY", "upstream-secret")
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
storage: {master_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
web: {admin_username: admin, admin_password: test}
upstream:
  poll_timeout: 45s
pool:
  keys:
    - api_key: "${TEST_TODOFOR_KEY}"
models:
  default: openai:vendor/upstream-model
  aliases:
    public-model: openai:vendor/upstream-model
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":8080" || cfg.Upstream.BaseURL != defaultBaseURL {
		t.Fatalf("defaults not applied: %#v", cfg)
	}
	if cfg.Upstream.PollTimeout != 45*time.Second {
		t.Fatalf("poll timeout = %s", cfg.Upstream.PollTimeout)
	}
	if cfg.Pool.Keys[0].APIKey != "upstream-secret" {
		t.Fatalf("API key was not expanded: %q", cfg.Pool.Keys[0].APIKey)
	}
	if got := cfg.Models.Resolve("public-model"); got != "openai:vendor/upstream-model" {
		t.Fatalf("resolved model = %q", got)
	}
	if !reflect.DeepEqual(cfg.ToolProtocol.DenyUpstreamTools, []string{"device:*", "cloud:*"}) {
		t.Fatalf("deny defaults = %#v", cfg.ToolProtocol.DenyUpstreamTools)
	}
}

func TestLoadMasterKeyFromYAML(t *testing.T) {
	want := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
storage:
  master_key: "` + base64.StdEncoding.EncodeToString(want) + `"
web: {admin_username: admin, admin_password: test}
models: {default: "openai:vendor/upstream-model"}
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.MasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("master key = %x, want %x", got, want)
	}
}

func TestLoadRejectsInvalidMasterKey(t *testing.T) {
	for name, value := range map[string]string{
		"missing":        "",
		"invalid_base64": "not-base64",
		"wrong_length":   base64.StdEncoding.EncodeToString(make([]byte, 31)),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			data := []byte("storage:\n  master_key: \"" + value + "\"\n" +
				"web: {admin_username: admin, admin_password: test}\n" +
				"models: {default: \"openai:vendor/upstream-model\"}\n")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "storage.master_key must be base64-encoded 32 bytes") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadRejectsEmptyExpandedClientToken(t *testing.T) {
	t.Setenv("MISSING_CLIENT_TOKEN", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
server:
  client_tokens: ["${MISSING_CLIENT_TOKEN}"]
storage: {master_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
web: {admin_username: admin, admin_password: test}
models: {default: "openai:vendor/upstream-model"}
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "server.client_tokens[0] must not be empty") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadTrimsAndRejectsDuplicateClientTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
server:
  client_tokens: [" token ", "token"]
storage: {master_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
web: {admin_username: admin, admin_password: test}
models: {default: "openai:vendor/upstream-model"}
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "duplicates an earlier token") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsInvalidTrustedProxyCIDR(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
storage: {master_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
web:
  admin_username: admin
  admin_password: test
  trusted_proxies: ["not-a-cidr"]
models: {default: "openai:vendor/upstream-model"}
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "web.trusted_proxies[0]") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadMergesAndDeduplicatesKeyFiles(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "accounts.keys")
	if err := os.WriteFile(keyFile, []byte("# imported accounts\nfile-key-1\ninline-key\n\nfile-key-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	data := []byte(`
storage: {master_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
web: {admin_username: admin, admin_password: test}
pool:
  key_files:
    - accounts.keys
  keys:
    - api_key: inline-key
      project_id: project-1
models:
  default: openai:vendor/upstream-model
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := cfg.LegacyPoolKeys()
	if err != nil {
		t.Fatal(err)
	}
	want := []AccountKey{
		{APIKey: "inline-key", ProjectID: "project-1"},
		{APIKey: "file-key-1"},
		{APIKey: "file-key-2"},
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %#v, want %#v", keys, want)
	}
}

func TestLegacyPoolKeysDoesNotRewriteSources(t *testing.T) {
	t.Setenv("REMOVE_ME", "remove-key")
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "accounts.keys")
	if err := os.WriteFile(keyFile, []byte("remove-key\nfile-keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	data := []byte(`
storage: {master_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
web: {admin_username: admin, admin_password: test}
pool:
  key_files:
    - accounts.keys
  keys:
    - api_key: "${REMOVE_ME}"
      project_id: removed-project
    - api_key: inline-keep
      project_id: kept-project
models:
  default: openai:vendor/upstream-model
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := cfg.LegacyPoolKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 {
		t.Fatalf("legacy keys = %#v", keys)
	}

	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	keyFileData, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configData), "${REMOVE_ME}") || !strings.Contains(string(keyFileData), "remove-key") {
		t.Fatalf("legacy sources were rewritten:\nconfig=%s\nkey file=%s", configData, keyFileData)
	}
}

func TestLoadRejectsMissingKeyFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	data := []byte(`
storage: {master_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
web: {admin_username: admin, admin_password: test}
pool:
  key_files: [missing.keys]
models:
  default: openai:vendor/upstream-model
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.LegacyPoolKeys(); err == nil || !strings.Contains(err.Error(), "open pool key file") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadPoolKeysRereadsKeyFiles(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "accounts.keys")
	if err := os.WriteFile(keyFile, []byte("file-key-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	data := []byte(`
storage: {master_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
web: {admin_username: admin, admin_password: test}
pool:
  key_files:
    - accounts.keys
  keys:
    - api_key: inline-key
models:
  default: openai:vendor/upstream-model
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("file-key-2\nfile-key-3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := cfg.LegacyPoolKeys()
	if err != nil {
		t.Fatal(err)
	}
	want := []AccountKey{
		{APIKey: "inline-key"},
		{APIKey: "file-key-2"},
		{APIKey: "file-key-3"},
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %#v, want %#v", keys, want)
	}
	// Loaded configuration keeps only inline migration candidates.
	if !reflect.DeepEqual(cfg.Pool.Keys, []AccountKey{{APIKey: "inline-key"}}) {
		t.Fatalf("loaded keys mutated: %#v", cfg.Pool.Keys)
	}
}

func TestRejectsUnqualifiedUpstreamModels(t *testing.T) {
	for name, models := range map[string]ModelsConfig{
		"default": {Default: "anthropic/claude-sonnet-4.6"},
		"alias": {
			Default: "openai:openai/gpt-5.6-sol",
			Aliases: map[string]string{"claude": "anthropic/claude-sonnet-4.6"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{
				Pool:    PoolConfig{Keys: []AccountKey{{APIKey: "key"}}},
				Storage: StorageConfig{MasterKey: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="},
				Models:  models,
			}
			cfg.setDefaults()
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected unqualified model validation error")
			}
		})
	}
}

func TestExplicitEmptyDenyListIsPreserved(t *testing.T) {
	cfg := Config{
		Pool:         PoolConfig{Keys: []AccountKey{{APIKey: "key"}}},
		Models:       ModelsConfig{Default: "model"},
		ToolProtocol: ToolProtocolConfig{DenyUpstreamTools: []string{}},
	}
	cfg.setDefaults()
	if cfg.ToolProtocol.DenyUpstreamTools == nil || len(cfg.ToolProtocol.DenyUpstreamTools) != 0 {
		t.Fatalf("explicit empty deny list was replaced: %#v", cfg.ToolProtocol.DenyUpstreamTools)
	}
}
