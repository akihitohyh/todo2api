package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"todo2api/internal/config"
	"todo2api/internal/upstream"
)

func TestRoundRobinAcrossAccounts(t *testing.T) {
	first := &Account{ProjectID: "project-1"}
	second := &Account{ProjectID: "project-2"}
	p := &Pool{
		accounts: []*Account{first, second},
		strategy: "round_robin",
	}

	want := []*Account{first, second, first, second, first}
	for i, expected := range want {
		if got := p.Pick(); got != expected {
			t.Fatalf("pick %d = %p, want %p", i, got, expected)
		}
	}

	if got := p.At(1); got != second {
		t.Fatalf("At(1) = %p, want %p", got, second)
	}
	if got := p.IndexOf(second); got != 1 {
		t.Fatalf("IndexOf(second) = %d, want 1", got)
	}
	if got := p.At(2); got != nil {
		t.Fatalf("At(2) = %p, want nil", got)
	}
	if got := p.IndexOf(&Account{}); got != -1 {
		t.Fatalf("IndexOf(unknown) = %d, want -1", got)
	}
}

func TestLeastBusySkipsAccountInFlight(t *testing.T) {
	first := &Account{ProjectID: "project-1"}
	second := &Account{ProjectID: "project-2"}
	p := &Pool{
		accounts: []*Account{first, second},
		strategy: "least_busy",
	}

	if got := p.Pick(); got != first {
		t.Fatalf("initial pick = %p, want first account %p", got, first)
	}

	first.Acquire()
	t.Cleanup(first.Release)
	if got := p.Pick(); got != second {
		t.Fatalf("pick while first is busy = %p, want second account %p", got, second)
	}

	second.Acquire()
	t.Cleanup(second.Release)
	if got := p.Pick(); got != first {
		t.Fatalf("pick on equal load = %p, want stable first account %p", got, first)
	}
}

func TestLeastBusyRotatesEqualAccountsAndSkipsCooldown(t *testing.T) {
	first := &Account{ProjectID: "project-1"}
	second := &Account{ProjectID: "project-2"}
	p := &Pool{
		accounts: []*Account{first, second},
		strategy: "least_busy",
	}

	if got := p.Pick(); got != first {
		t.Fatalf("first equal-load pick = %p, want first account %p", got, first)
	}
	if got := p.Pick(); got != second {
		t.Fatalf("second equal-load pick = %p, want second account %p", got, second)
	}

	first.CoolDown(time.Hour)
	for i := 0; i < 3; i++ {
		if got := p.Pick(); got != second {
			t.Fatalf("pick %d during cooldown = %p, want second account %p", i, got, second)
		}
	}
	second.CoolDown(time.Hour)
	if got := p.Pick(); got != nil {
		t.Fatalf("all-cooled pick = %p, want nil", got)
	}
}

func TestEmptyPoolReturnsNil(t *testing.T) {
	if got := (&Pool{}).Pick(); got != nil {
		t.Fatalf("empty pool pick = %p, want nil", got)
	}
}

func TestCommonModelsAndResolution(t *testing.T) {
	first := []upstream.ModelInfo{
		{ID: "openai/gpt-5.6-sol", OwnedBy: "openai"},
		{ID: "anthropic/claude-sonnet-4.6", OwnedBy: "anthropic"},
		{ID: "only/first"},
	}
	second := []upstream.ModelInfo{
		{ID: "anthropic/claude-sonnet-4.6"},
		{ID: "openai/gpt-5.6-sol"},
		{ID: "only/second"},
	}
	models := commonModels([][]upstream.ModelInfo{first, second})
	gotIDs := []string{models[0].ID, models[1].ID}
	wantIDs := []string{"anthropic/claude-sonnet-4.6", "openai/gpt-5.6-sol"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("common model IDs = %#v", gotIDs)
	}

	p := &Pool{}
	p.setModels(models)
	gotPublicIDs := []string{p.Models()[0].ID, p.Models()[1].ID}
	wantPublicIDs := []string{"claude-sonnet-4.6", "gpt-5.6-sol"}
	if !reflect.DeepEqual(gotPublicIDs, wantPublicIDs) {
		t.Fatalf("public model IDs = %#v, want %#v", gotPublicIDs, wantPublicIDs)
	}
	if got := p.ResolveModel("claude-sonnet-4.6"); got != "anthropic:anthropic/claude-sonnet-4.6" {
		t.Fatalf("resolved short model = %q", got)
	}
	if got := p.ResolveModel("anthropic/claude-sonnet-4.6"); got != "anthropic:anthropic/claude-sonnet-4.6" {
		t.Fatalf("resolved model = %q", got)
	}
	if got := p.ResolveModel("private:model"); got != "private:model" {
		t.Fatalf("unknown model changed to %q", got)
	}
	if model, ok := p.Model("openai:openai/gpt-5.6-sol"); !ok || model.ID != "openai/gpt-5.6-sol" {
		t.Fatalf("runner model lookup = %#v, %v", model, ok)
	}
	if publicID, ok := p.PublicModelID("openai:openai/gpt-5.6-sol"); !ok || publicID != "gpt-5.6-sol" {
		t.Fatalf("public model lookup = %q, %v", publicID, ok)
	}
	if len(p.Models()) != 2 {
		t.Fatalf("models = %#v", p.Models())
	}
}

func TestPublicModelIDsDisambiguateProviderCollisions(t *testing.T) {
	p := &Pool{}
	p.setModels([]upstream.ModelInfo{
		{ID: "anthropic/shared-model"},
		{ID: "google/shared-model"},
		{ID: "openai/unique-model"},
	})

	got := []string{p.Models()[0].ID, p.Models()[1].ID, p.Models()[2].ID}
	want := []string{"anthropic/shared-model", "google/shared-model", "unique-model"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("public model IDs = %#v, want %#v", got, want)
	}
	if got := p.ResolveModel("anthropic/shared-model"); got != "anthropic:anthropic/shared-model" {
		t.Fatalf("resolved colliding model = %q", got)
	}
	if got := p.ResolveModel("shared-model"); got != "shared-model" {
		t.Fatalf("ambiguous short model unexpectedly resolved to %q", got)
	}
}

func TestCommonModelsWithoutSuccessfulCatalog(t *testing.T) {
	if models := commonModels(nil); models != nil {
		t.Fatalf("models = %#v", models)
	}
}

func TestNewKeepsAccountWhenModelDiscoveryFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
		case "/api/v1/agents":
			json.NewEncoder(w).Encode([]map[string]any{{"id": "agent-1", "model": "template:model"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p, err := New(&config.Config{
		Upstream: config.UpstreamConfig{BaseURL: server.URL + "/api/v1", PollTimeout: time.Second},
		Pool: config.PoolConfig{Strategy: "round_robin", Keys: []config.AccountKey{{
			APIKey: "key", ProjectID: "project-1",
		}}},
		Models: config.ModelsConfig{Default: "openai:openai/gpt-5.6-sol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Pick() == nil || len(p.Models()) != 0 || len(p.Warnings()) != 1 {
		t.Fatalf("account=%#v models=%#v warnings=%#v", p.Pick(), p.Models(), p.Warnings())
	}
}

func TestNewHidesDynamicModelsWhenAnyAccountDiscoveryFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			if r.Header.Get("X-API-Key") == "failing-key" {
				http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]any{{"id": "openai/gpt-5.6-sol"}},
			})
		case "/api/v1/agents":
			json.NewEncoder(w).Encode([]map[string]any{{"id": "agent-1", "model": "template:model"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p, err := New(&config.Config{
		Upstream: config.UpstreamConfig{BaseURL: server.URL + "/api/v1", PollTimeout: time.Second},
		Pool: config.PoolConfig{Strategy: "round_robin", Keys: []config.AccountKey{
			{APIKey: "working-key", ProjectID: "project-1"},
			{APIKey: "failing-key", ProjectID: "project-2"},
		}},
		Models: config.ModelsConfig{Default: "openai:openai/gpt-5.6-sol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Models()) != 0 || len(p.Warnings()) != 1 {
		t.Fatalf("models=%#v warnings=%#v", p.Models(), p.Warnings())
	}
}

func TestNewSkipsUnusableAccountsAndPreservesOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "bad-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/models":
			json.NewEncoder(w).Encode(map[string]any{
				"object": "list", "data": []map[string]any{{"id": "openai/gpt-5.6-sol"}},
			})
		case "/api/v1/projects":
			// Delay the first configured key so concurrent completion order differs
			// from configuration order.
			if key == "first-key" {
				time.Sleep(30 * time.Millisecond)
			}
			json.NewEncoder(w).Encode([]map[string]any{{"id": "project-" + key}})
		case "/api/v1/agents":
			json.NewEncoder(w).Encode([]map[string]any{{"id": "agent-" + key}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p, err := New(&config.Config{
		Upstream: config.UpstreamConfig{BaseURL: server.URL + "/api/v1", PollTimeout: time.Second},
		Pool: config.PoolConfig{Strategy: "round_robin", Keys: []config.AccountKey{
			{APIKey: "first-key"},
			{APIKey: "bad-key"},
			{APIKey: "third-key"},
		}},
		Models: config.ModelsConfig{Default: "openai:openai/gpt-5.6-sol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Len() != 2 || len(p.Warnings()) != 1 {
		t.Fatalf("accounts=%d warnings=%#v", p.Len(), p.Warnings())
	}
	if first, second := p.Pick(), p.Pick(); first.ProjectID != "project-first-key" || second.ProjectID != "project-third-key" {
		t.Fatalf("account order = %q, %q", first.ProjectID, second.ProjectID)
	}
	if len(p.Models()) != 1 || p.Models()[0].ID != "gpt-5.6-sol" {
		t.Fatalf("models = %#v", p.Models())
	}
}

func TestNewFailsWhenEveryAccountIsUnusable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := New(&config.Config{
		Upstream: config.UpstreamConfig{BaseURL: server.URL, PollTimeout: time.Second},
		Pool: config.PoolConfig{Keys: []config.AccountKey{
			{APIKey: "bad-key-1"}, {APIKey: "bad-key-2"},
		}},
		Models: config.ModelsConfig{Default: "openai:openai/gpt-5.6-sol"},
	})
	if err == nil || !strings.Contains(err.Error(), "no usable accounts out of 2 configured") {
		t.Fatalf("error = %v", err)
	}
}

func TestWarmAddsAccountsWithoutChangingExistingIndexes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		switch r.URL.Path {
		case "/api/v1/models":
			json.NewEncoder(w).Encode(map[string]any{
				"object": "list", "data": []map[string]any{{"id": "openai/gpt-5.6-sol"}},
			})
		case "/api/v1/projects":
			json.NewEncoder(w).Encode([]map[string]any{{"id": "project-" + key}})
		case "/api/v1/agents":
			json.NewEncoder(w).Encode([]map[string]any{{"id": "agent-" + key}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	keys := make([]config.AccountKey, bootstrapAccounts+3)
	for i := range keys {
		keys[i].APIKey = fmt.Sprintf("key-%d", i)
	}
	p, err := New(&config.Config{
		Upstream: config.UpstreamConfig{BaseURL: server.URL + "/api/v1", PollTimeout: time.Second},
		Pool:     config.PoolConfig{Strategy: "round_robin", Keys: keys},
		Models:   config.ModelsConfig{Default: "openai:openai/gpt-5.6-sol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := p.At(0)
	if p.Len() != bootstrapAccounts || p.Configured() != len(keys) {
		t.Fatalf("ready=%d configured=%d", p.Len(), p.Configured())
	}

	var callbacks int
	p.Warm(context.Background(), func(ready, skipped, processed int) {
		callbacks++
		if skipped != 0 || processed > len(keys) || ready <= bootstrapAccounts {
			t.Fatalf("callback = ready %d, skipped %d, processed %d", ready, skipped, processed)
		}
		if ready != processed {
			t.Fatalf("all test accounts should be ready: ready %d, processed %d", ready, processed)
		}
	})
	if callbacks == 0 || p.Len() != len(keys) {
		t.Fatalf("callbacks=%d ready=%d", callbacks, p.Len())
	}
	if p.At(0) != first || p.IndexOf(first) != 0 {
		t.Fatal("background warmup changed an existing account index")
	}
}

func TestReloadKeysAddsRemovesAndRestores(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		switch r.URL.Path {
		case "/api/v1/models":
			json.NewEncoder(w).Encode(map[string]any{
				"object": "list", "data": []map[string]any{{"id": "openai/gpt-5.6-sol"}},
			})
		case "/api/v1/projects":
			json.NewEncoder(w).Encode([]map[string]any{{"id": "project-" + key}})
		case "/api/v1/agents":
			json.NewEncoder(w).Encode([]map[string]any{{"id": "agent-" + key}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := &config.Config{
		Upstream: config.UpstreamConfig{BaseURL: server.URL + "/api/v1", PollTimeout: time.Second},
		Pool: config.PoolConfig{Strategy: "round_robin", Keys: []config.AccountKey{
			{APIKey: "key-a"},
			{APIKey: "key-b"},
		}},
		Models: config.ModelsConfig{Default: "openai:openai/gpt-5.6-sol"},
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first := p.At(0)
	second := p.At(1)
	if first == nil || second == nil {
		t.Fatalf("expected two ready accounts, got %d", p.Len())
	}
	firstIndex := p.IndexOf(first)
	secondIndex := p.IndexOf(second)

	stats, err := p.ReloadKeys(context.Background(), []config.AccountKey{
		{APIKey: "key-a"},
		{APIKey: "key-c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Added != 1 || stats.Removed != 1 || stats.Restored != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if p.Len() != 2 || p.Configured() != 2 {
		t.Fatalf("ready=%d configured=%d", p.Len(), p.Configured())
	}
	if p.At(firstIndex) != first || first.Removed() {
		t.Fatal("kept account lost index or was removed")
	}
	if p.At(secondIndex) != second || !second.Removed() {
		t.Fatal("removed account should remain addressable but marked removed")
	}
	if got := p.Pick(); got == second {
		t.Fatal("Pick selected a removed account")
	}

	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		acc := p.Pick()
		if acc == nil || acc.Removed() {
			t.Fatalf("pick returned unusable account: %#v", acc)
		}
		seen[acc.APIKey()] = true
	}
	if !seen["key-a"] || !seen["key-c"] || seen["key-b"] {
		t.Fatalf("pick set = %#v", seen)
	}

	stats, err = p.ReloadKeys(context.Background(), []config.AccountKey{
		{APIKey: "key-a"},
		{APIKey: "key-b"},
		{APIKey: "key-c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Restored != 1 || stats.Added != 0 || stats.Removed != 0 {
		t.Fatalf("restore stats = %+v", stats)
	}
	if second.Removed() || p.IndexOf(second) != secondIndex {
		t.Fatal("restored account should reuse the original index")
	}
	if p.Len() != 3 {
		t.Fatalf("ready after restore = %d", p.Len())
	}
}

func TestReloadKeysPreservesInlineOnlySet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			json.NewEncoder(w).Encode(map[string]any{
				"object": "list", "data": []map[string]any{{"id": "openai/gpt-5.6-sol"}},
			})
		case "/api/v1/projects":
			json.NewEncoder(w).Encode([]map[string]any{{"id": "project-1"}})
		case "/api/v1/agents":
			json.NewEncoder(w).Encode([]map[string]any{{"id": "agent-1"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p, err := New(&config.Config{
		Upstream: config.UpstreamConfig{BaseURL: server.URL + "/api/v1", PollTimeout: time.Second},
		Pool:     config.PoolConfig{Keys: []config.AccountKey{{APIKey: "only-key"}, {APIKey: "file-key"}}},
		Models:   config.ModelsConfig{Default: "openai:openai/gpt-5.6-sol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := p.ReloadKeys(context.Background(), []config.AccountKey{{APIKey: "only-key"}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 || p.Len() != 1 || p.Configured() != 1 {
		t.Fatalf("stats=%+v ready=%d configured=%d", stats, p.Len(), p.Configured())
	}
}
