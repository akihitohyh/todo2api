package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"todo2api/internal/config"
)

func testConfig(t *testing.T, dir string) *config.Config {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	data := `
storage:
  path: data/test.db
  master_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
web:
  admin_username: admin
  admin_password: test
pool:
  keys:
    - api_key: sk-legacy-secret
models:
  default: openai:openai/test
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestEncryptedLegacyImportRunsOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 1)
	}

	store, err := Open(ctx, cfg, master)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := store.Accounts(ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}
	if accounts[0].APIKey != "sk-legacy-secret" {
		t.Fatalf("api key=%q", accounts[0].APIKey)
	}
	if err := store.DeleteAccount(ctx, accounts[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(cfg.Storage.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-legacy-secret") {
		t.Fatal("database contains plaintext api key")
	}

	store, err = Open(ctx, cfg, master)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accounts, err = store.Accounts(ctx)
	if err != nil || len(accounts) != 0 {
		t.Fatalf("legacy key was reimported: %v err=%v", accounts, err)
	}
}

func TestWrongMasterKeyFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	first := make([]byte, 32)
	second := make([]byte, 32)
	second[0] = 1
	store, err := Open(ctx, cfg, first)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if _, err := Open(ctx, cfg, second); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong-key err=%v", err)
	}
}

func TestCiphertextUsesRandomNonceAndFingerprintDeduplicates(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, t.TempDir())
	cfg.Pool.Keys = nil
	store, err := Open(ctx, cfg, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.CreateAccount(ctx, config.AccountKey{APIKey: "sk-same-prefix-one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateAccount(ctx, config.AccountKey{APIKey: "sk-same-prefix-two"})
	if err != nil {
		t.Fatal(err)
	}
	var firstNonce, secondNonce []byte
	if err := store.db.QueryRowContext(ctx, `SELECT key_nonce FROM accounts WHERE id=?`, first.ID).Scan(&firstNonce); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT key_nonce FROM accounts WHERE id=?`, second.ID).Scan(&secondNonce); err != nil {
		t.Fatal(err)
	}
	if string(firstNonce) == string(secondNonce) {
		t.Fatal("different accounts reused an AES-GCM nonce")
	}
	if _, err := store.CreateAccount(ctx, config.AccountKey{APIKey: first.APIKey}); err == nil {
		t.Fatalf("duplicate key err=%v", err)
	}
}

func TestCorruptCiphertextFailsOnReopen(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, t.TempDir())
	master := make([]byte, 32)
	store, err := Open(ctx, cfg, master)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE accounts SET key_ciphertext=x'01'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, cfg, master)
	if err == nil || !strings.Contains(err.Error(), "validate encrypted accounts") {
		t.Fatalf("corrupt ciphertext err=%v", err)
	}
}

func TestEmptyCollectionsEncodeAsArrays(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, t.TempDir())
	cfg.Pool.Keys = nil
	store, err := Open(ctx, cfg, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accounts, err := store.Accounts(ctx)
	if err != nil || accounts == nil || len(accounts) != 0 {
		t.Fatalf("accounts=%#v err=%v", accounts, err)
	}
	stats, err := store.ModelStats(ctx, 30)
	if err != nil || stats.Models == nil {
		t.Fatalf("stats=%#v err=%v", stats, err)
	}
}

func TestConcurrentStatsWritesAndAggregation(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, t.TempDir())
	cfg.Pool.Keys = nil
	store, err := Open(ctx, cfg, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const writes = 12
	var wg sync.WaitGroup
	for i := range writes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := store.RecordCall(ctx, "model-a", Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 1, Cost: 0.25}, i%2 == 0)
			if err != nil {
				t.Errorf("record call %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	dashboard, err := store.Dashboard(ctx)
	if err != nil || dashboard.TotalCalls != writes {
		t.Fatalf("dashboard=%#v err=%v", dashboard, err)
	}
	for _, days := range []int{7, 30} {
		stats, err := store.ModelStats(ctx, days)
		if err != nil || len(stats.Models) != 1 || len(stats.Daily["model-a"]) != 1 {
			t.Fatalf("days=%d stats=%#v err=%v", days, stats, err)
		}
		point := stats.Daily["model-a"][0]
		if point.Calls != writes || point.InputTokens != writes*14 || point.OutputTokens != writes*2 || point.Credits != writes*0.25 {
			t.Fatalf("days=%d point=%#v", days, point)
		}
	}
}

func TestDashboardAccountStatusCountsMatchAPICategories(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, t.TempDir())
	cfg.Pool.Keys = nil
	store, err := Open(ctx, cfg, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	statuses := []struct {
		status         string
		disabledReason string
	}{
		{status: "active"},
		{status: "invalid"},
		{status: "error"},
		{status: "initializing"},
		{status: "exhausted", disabledReason: "exhausted"},
		{status: "error", disabledReason: "manual"},
	}
	for i, item := range statuses {
		a, err := store.CreateAccount(ctx, config.AccountKey{APIKey: fmt.Sprintf("status-key-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateHealth(ctx, a.ID, HealthUpdate{
			Status: item.status, Disable: item.disabledReason != "", DisabledReason: item.disabledReason,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountCount != 6 || got.ActiveCount != 1 || got.ExhaustedCount != 1 ||
		got.InvalidCount != 1 || got.ErrorCount != 1 || got.InitializingCount != 1 || got.DisabledCount != 1 {
		t.Fatalf("dashboard status counts = %+v", got)
	}
}

func TestOpenPreservesExistingParentModeAndSecuresSQLiteFiles(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, dir)
	cfg.Storage.Path = filepath.Join(parent, "todo2api.db")
	cfg.Pool.Keys = nil
	store, err := Open(context.Background(), cfg, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	info, err := os.Stat(parent)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("parent mode=%v err=%v", info.Mode().Perm(), err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(cfg.Storage.Path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%v err=%v", suffix, info.Mode().Perm(), err)
		}
	}
}

func TestOpenAppliesPendingSchemaMigrations(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	cfg.Pool.Keys = nil
	if err := os.MkdirAll(filepath.Dir(cfg.Storage.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", cfg.Storage.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range schemaMigrations[0].statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(1,unixepoch())`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(context.Background(), cfg, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if want := schemaMigrations[len(schemaMigrations)-1].version; version != want {
		t.Fatalf("schema version=%d want=%d", version, want)
	}
	var indexName string
	if err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='daily_model_stats_day_idx'`).Scan(&indexName); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsSchemaMigrationGap(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	cfg.Pool.Keys = nil
	if err := os.MkdirAll(filepath.Dir(cfg.Storage.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", cfg.Storage.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
		INSERT INTO schema_migrations(version,applied_at) VALUES(2,unixepoch())`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), cfg, make([]byte, 32)); err == nil || !strings.Contains(err.Error(), "not contiguous") {
		t.Fatalf("error = %v", err)
	}
}
