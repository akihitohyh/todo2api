package config

import (
	"os"
	"path/filepath"
	"reflect"
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
