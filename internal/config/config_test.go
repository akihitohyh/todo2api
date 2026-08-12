package config

import (
	"bytes"
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

func TestLoadMergesAndDeduplicatesKeyFiles(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "accounts.keys")
	if err := os.WriteFile(keyFile, []byte("# imported accounts\nfile-key-1\ninline-key\n\nfile-key-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	data := []byte(`
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
	want := []AccountKey{
		{APIKey: "inline-key", ProjectID: "project-1"},
		{APIKey: "file-key-1"},
		{APIKey: "file-key-2"},
	}
	if !reflect.DeepEqual(cfg.Pool.Keys, want) {
		t.Fatalf("keys = %#v, want %#v", cfg.Pool.Keys, want)
	}
}

func TestRemovePoolKeyPersistsAcrossReload(t *testing.T) {
	t.Setenv("REMOVE_ME", "remove-key")
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "accounts.keys")
	if err := os.WriteFile(keyFile, []byte("remove-key\nfile-keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	data := []byte(`
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
	if err := cfg.RemovePoolKey("remove-key"); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Pool.Keys) != 2 {
		t.Fatalf("in-memory keys after removal = %#v", cfg.Pool.Keys)
	}

	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	keyFileData, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(configData, []byte("${REMOVE_ME}")) || bytes.Contains(keyFileData, []byte("remove-key")) {
		t.Fatalf("key remained in sources:\nconfig=%s\nkey file=%s", configData, keyFileData)
	}

	reloaded, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []AccountKey{
		{APIKey: "inline-keep", ProjectID: "kept-project"},
		{APIKey: "file-keep"},
	}
	if !reflect.DeepEqual(reloaded.Pool.Keys, want) {
		t.Fatalf("reloaded keys = %#v, want %#v", reloaded.Pool.Keys, want)
	}
}

func TestLoadRejectsMissingKeyFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	data := []byte(`
pool:
  key_files: [missing.keys]
models:
  default: openai:vendor/upstream-model
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "open pool key file") {
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
	keys, err := cfg.LoadPoolKeys()
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
	// Original loaded config stays unchanged until the pool reloads.
	if !reflect.DeepEqual(cfg.Pool.Keys, []AccountKey{{APIKey: "inline-key"}, {APIKey: "file-key-1"}}) {
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
				Pool:   PoolConfig{Keys: []AccountKey{{APIKey: "key"}}},
				Models: models,
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
