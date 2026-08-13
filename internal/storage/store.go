package storage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"todo2api/internal/config"
)

const (
	legacyMigrationKey = "legacy_keys_imported"
	masterVerifierKey  = "master_key_verifier"
)

type schemaMigration struct {
	version    int
	statements []string
}

var schemaMigrations = []schemaMigration{
	{version: 1, statements: []string{
		`CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key_ciphertext BLOB NOT NULL, key_nonce BLOB NOT NULL, key_version INTEGER NOT NULL DEFAULT 1,
			key_fingerprint TEXT NOT NULL UNIQUE, key_masked TEXT NOT NULL,
			project_id TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1, disabled_reason TEXT NOT NULL DEFAULT '',
			health_status TEXT NOT NULL DEFAULT 'initializing', balance REAL NOT NULL DEFAULT 0,
			balance_unlimited INTEGER NOT NULL DEFAULT 0, subscription_tier TEXT NOT NULL DEFAULT '',
			balance_at INTEGER, last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX accounts_enabled_idx ON accounts(enabled)`,
		`CREATE TABLE admin_sessions (
			token_hash BLOB PRIMARY KEY, expires_at INTEGER NOT NULL, created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX admin_sessions_expiry_idx ON admin_sessions(expires_at)`,
		`CREATE TABLE daily_model_stats (
			day TEXT NOT NULL, model TEXT NOT NULL, calls INTEGER NOT NULL DEFAULT 0,
			successes INTEGER NOT NULL DEFAULT 0, failures INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			credits REAL NOT NULL DEFAULT 0, PRIMARY KEY(day, model)
		)`,
	}},
	{version: 2, statements: []string{
		`CREATE INDEX daily_model_stats_day_idx ON daily_model_stats(day)`,
	}},
}

type Store struct {
	db      *sql.DB
	aead    cipher.AEAD
	hmacKey []byte
}

type Account struct {
	ID               int64      `json:"id"`
	APIKey           string     `json:"-"`
	APIKeyMasked     string     `json:"api_key_masked"`
	ProjectID        string     `json:"project_id,omitempty"`
	AgentID          string     `json:"agent_id,omitempty"`
	Enabled          bool       `json:"enabled"`
	DisabledReason   string     `json:"disabled_reason,omitempty"`
	HealthStatus     string     `json:"health_status"`
	Balance          float64    `json:"balance"`
	BalanceUnlimited bool       `json:"balance_unlimited"`
	SubscriptionTier string     `json:"subscription_tier,omitempty"`
	BalanceAt        *time.Time `json:"balance_at,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type HealthUpdate struct {
	Status           string
	Balance          float64
	BalanceUnlimited bool
	SubscriptionTier string
	Error            string
	Disable          bool
	DisabledReason   string
}

type DashboardStats struct {
	AccountCount      int       `json:"account_count"`
	ActiveCount       int       `json:"active_count"`
	ExhaustedCount    int       `json:"exhausted_count"`
	InvalidCount      int       `json:"invalid_count"`
	ErrorCount        int       `json:"error_count"`
	InitializingCount int       `json:"initializing_count"`
	DisabledCount     int       `json:"disabled_count"`
	TotalBalance      float64   `json:"total_balance"`
	UnlimitedCount    int       `json:"unlimited_count"`
	TotalCalls        int64     `json:"total_calls"`
	DailyCalls        []DayCall `json:"daily_calls"`
}

type DayCall struct {
	Date  string `json:"date"`
	Count int64  `json:"count"`
}

type ModelPoint struct {
	Date         string  `json:"date"`
	Calls        int64   `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Credits      float64 `json:"credits"`
}

type ModelStats struct {
	Models []string                `json:"models"`
	Daily  map[string][]ModelPoint `json:"daily"`
}

type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	Cost             float64
}

func Open(ctx context.Context, cfg *config.Config, masterKey []byte) (*Store, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes")
	}
	if err := ensureStoragePath(cfg.Storage.Path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", cfg.Storage.Path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	cleanup := func(err error) (*Store, error) { _ = db.Close(); return nil, err }
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		return cleanup(fmt.Errorf("configure sqlite: %w", err))
	}
	block, err := aes.NewCipher(derive(masterKey, "todo2api/account-encryption/v1"))
	if err != nil {
		return cleanup(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return cleanup(err)
	}
	s := &Store{db: db, aead: aead, hmacKey: derive(masterKey, "todo2api/account-fingerprint/v1")}
	if err := s.migrate(ctx); err != nil {
		return cleanup(err)
	}
	if err := s.verifyMasterKey(ctx, masterKey); err != nil {
		return cleanup(err)
	}
	if err := s.importLegacyOnce(ctx, cfg); err != nil {
		return cleanup(err)
	}
	if _, err := s.Accounts(ctx); err != nil {
		return cleanup(fmt.Errorf("validate encrypted accounts: %w", err))
	}
	if err := secureSQLiteFiles(cfg.Storage.Path); err != nil {
		return cleanup(err)
	}
	return s, nil
}

func ensureStoragePath(path string) error {
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create storage directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure storage directory: %w", err)
		}
	case err != nil:
		return fmt.Errorf("inspect storage directory: %w", err)
	case !info.IsDir():
		return fmt.Errorf("storage parent is not a directory: %s", dir)
	}

	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("create sqlite file: %w", err)
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("close sqlite file: %w", closeErr)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure sqlite file: %w", err)
	}
	return nil
}

func secureSQLiteFiles(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		candidate := path + suffix
		if err := os.Chmod(candidate, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure sqlite file %s: %w", candidate, err)
		}
	}
	return nil
}

func derive(master []byte, label string) []byte {
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte(label))
	return mac.Sum(nil)
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("initialize schema migrations: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read schema versions: %w", err)
	}
	current := 0
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return fmt.Errorf("read schema version: %w", err)
		}
		if version != current+1 {
			rows.Close()
			return fmt.Errorf("database schema migrations are not contiguous at version %d", version)
		}
		current = version
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate schema versions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close schema version rows: %w", err)
	}
	latest := schemaMigrations[len(schemaMigrations)-1].version
	if current > latest {
		return fmt.Errorf("database schema version %d is newer than supported version %d", current, latest)
	}
	for _, migration := range schemaMigrations {
		if migration.version <= current {
			continue
		}
		for _, statement := range migration.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply schema migration %d: %w", migration.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, unixepoch())`, migration.version); err != nil {
			return fmt.Errorf("record schema migration %d: %w", migration.version, err)
		}
	}
	return tx.Commit()
}

func (s *Store) verifyMasterKey(ctx context.Context, master []byte) error {
	want := base64.StdEncoding.EncodeToString(derive(master, "todo2api/master-verifier/v1"))
	var got string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, masterVerifierKey).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO metadata(key,value) VALUES(?,?)`, masterVerifierKey, want); err != nil {
			return err
		}
		if err = s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, masterVerifierKey).Scan(&got); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(got), []byte(want)) {
		return fmt.Errorf("storage.master_key does not match this database")
	}
	return nil
}

func (s *Store) importLegacyOnce(ctx context.Context, cfg *config.Config) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var value string
	err = tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, legacyMigrationKey).Scan(&value)
	if err == nil {
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var accountCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&accountCount); err != nil {
		return err
	}
	if accountCount > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO metadata(key,value) VALUES(?,?)`, legacyMigrationKey, "1"); err != nil {
			return err
		}
		return tx.Commit()
	}
	keys, err := cfg.LegacyPoolKeys()
	if err != nil {
		return fmt.Errorf("read legacy account keys: %w", err)
	}
	for _, key := range keys {
		if _, err := s.createAccount(ctx, tx, key); err != nil && !isUnique(err) {
			return fmt.Errorf("import legacy account: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO metadata(key,value) VALUES(?,?)`, legacyMigrationKey, "1"); err != nil {
		return err
	}
	return tx.Commit()
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) createAccount(ctx context.Context, db execer, key config.AccountKey) (int64, error) {
	key.APIKey = strings.TrimSpace(key.APIKey)
	if key.APIKey == "" {
		return 0, fmt.Errorf("api key must not be empty")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return 0, err
	}
	ciphertext := s.aead.Seal(nil, nonce, []byte(key.APIKey), nil)
	now := time.Now().UTC().Unix()
	result, err := db.ExecContext(ctx, `INSERT INTO accounts(
		key_ciphertext,key_nonce,key_fingerprint,key_masked,project_id,agent_id,enabled,created_at,updated_at
	) VALUES(?,?,?,?,?,?,?,?,?)`, ciphertext, nonce, s.fingerprint(key.APIKey), maskKey(key.APIKey),
		strings.TrimSpace(key.ProjectID), strings.TrimSpace(key.AgentID), 1, now, now)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) CreateAccount(ctx context.Context, key config.AccountKey) (Account, error) {
	id, err := s.createAccount(ctx, s.db, key)
	if err != nil {
		if isUnique(err) {
			return Account{}, fmt.Errorf("api key already exists")
		}
		return Account{}, err
	}
	return s.Account(ctx, id)
}

func isUnique(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

func (s *Store) fingerprint(key string) string {
	mac := hmac.New(sha256.New, s.hmacKey)
	_, _ = mac.Write([]byte(key))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", min(16, len(key)-8)) + key[len(key)-4:]
}

func (s *Store) scanAccount(scanner interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var ciphertext, nonce []byte
	var enabled, unlimited int
	var balanceAt sql.NullInt64
	var createdAt, updatedAt int64
	err := scanner.Scan(&a.ID, &ciphertext, &nonce, &a.APIKeyMasked, &a.ProjectID, &a.AgentID,
		&enabled, &a.DisabledReason, &a.HealthStatus, &a.Balance, &unlimited,
		&a.SubscriptionTier, &balanceAt, &a.LastError, &createdAt, &updatedAt)
	if err != nil {
		return a, err
	}
	plain, err := s.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return a, fmt.Errorf("decrypt account %d: invalid master key or data", a.ID)
	}
	a.APIKey = string(plain)
	a.Enabled = enabled != 0
	a.BalanceUnlimited = unlimited != 0
	a.CreatedAt = time.Unix(createdAt, 0).UTC()
	a.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	if balanceAt.Valid {
		t := time.Unix(balanceAt.Int64, 0).UTC()
		a.BalanceAt = &t
	}
	return a, nil
}

const accountColumns = `id,key_ciphertext,key_nonce,key_masked,project_id,agent_id,enabled,
	disabled_reason,health_status,balance,balance_unlimited,subscription_tier,balance_at,last_error,created_at,updated_at`

func (s *Store) Account(ctx context.Context, id int64) (Account, error) {
	a, err := s.scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("account not found")
	}
	return a, err
}

func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Account, 0)
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) PoolKeys(ctx context.Context) ([]config.AccountKey, error) {
	accounts, err := s.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]config.AccountKey, 0, len(accounts))
	for _, a := range accounts {
		keys = append(keys, config.AccountKey{ID: a.ID, APIKey: a.APIKey, ProjectID: a.ProjectID, AgentID: a.AgentID, Enabled: a.Enabled})
	}
	return keys, nil
}

func (s *Store) EnabledPoolKeys(ctx context.Context) ([]config.AccountKey, error) {
	keys, err := s.PoolKeys(ctx)
	if err != nil {
		return nil, err
	}
	active := keys[:0]
	for _, key := range keys {
		if key.Enabled {
			active = append(active, key)
		}
	}
	return active, nil
}

func (s *Store) SetEnabled(ctx context.Context, id int64, enabled bool, reason string) error {
	if enabled {
		reason = ""
	}
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET enabled=?,disabled_reason=?,updated_at=unixepoch() WHERE id=?`, enabled, reason, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("account not found")
	}
	return nil
}

func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("account not found")
	}
	return nil
}

func (s *Store) UpdateHealth(ctx context.Context, id int64, update HealthUpdate) error {
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET health_status=?,balance=?,balance_unlimited=?,
		subscription_tier=?,balance_at=unixepoch(),last_error=?,enabled=CASE WHEN ?=1 THEN 0 ELSE enabled END,
		disabled_reason=CASE WHEN ?=1 THEN ? ELSE disabled_reason END,updated_at=unixepoch() WHERE id=?`,
		update.Status, update.Balance, update.BalanceUnlimited, update.SubscriptionTier, update.Error,
		update.Disable, update.Disable, update.DisabledReason, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("account not found")
	}
	return nil
}

func (s *Store) SetHealthError(ctx context.Context, id int64, status, message string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET health_status=?,last_error=?,updated_at=unixepoch() WHERE id=?`, status, message, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("account not found")
	}
	return nil
}

func tokenHash(token string) []byte { sum := sha256.Sum256([]byte(token)); return sum[:] }

func (s *Store) CreateAdminSession(ctx context.Context, ttl time.Duration) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin_sessions(token_hash,expires_at,created_at) VALUES(?,?,?)`,
		tokenHash(token), now.Add(ttl).Unix(), now.Unix())
	return token, err
}

func (s *Store) ValidAdminSession(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM admin_sessions WHERE token_hash=? AND expires_at>unixepoch()`, tokenHash(token)).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) DeleteAdminSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token_hash=?`, tokenHash(token))
	return err
}

func (s *Store) CleanupSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at<=unixepoch()`)
	return err
}

func (s *Store) RecordCall(ctx context.Context, model string, usage Usage, success bool) error {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "unknown"
	}
	ok, failed := 0, 0
	if success {
		ok = 1
	} else {
		failed = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO daily_model_stats(
		day,model,calls,successes,failures,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,credits
	) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(day,model) DO UPDATE SET
		calls=calls+1,successes=successes+excluded.successes,failures=failures+excluded.failures,
		input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,
		cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,cache_write_tokens=cache_write_tokens+excluded.cache_write_tokens,
		credits=credits+excluded.credits`, time.Now().UTC().Format("2006-01-02"), model, 1, ok, failed,
		usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens, usage.Cost)
	return err
}

func (s *Store) Dashboard(ctx context.Context) (DashboardStats, error) {
	d := DashboardStats{DailyCalls: make([]DayCall, 0)}
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN enabled=1 AND health_status='active' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN disabled_reason='exhausted' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN enabled=1 AND health_status='invalid' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN enabled=1 AND health_status='error' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN enabled=1 AND health_status='initializing' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN enabled=0 AND disabled_reason!='exhausted' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(balance),0),COALESCE(SUM(balance_unlimited),0) FROM accounts`).Scan(
		&d.AccountCount, &d.ActiveCount, &d.ExhaustedCount, &d.InvalidCount, &d.ErrorCount, &d.InitializingCount,
		&d.DisabledCount, &d.TotalBalance, &d.UnlimitedCount)
	if err != nil {
		return d, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(calls),0) FROM daily_model_stats`).Scan(&d.TotalCalls); err != nil {
		return d, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT day,SUM(calls) FROM daily_model_stats GROUP BY day ORDER BY day DESC LIMIT 30`)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var p DayCall
		if err := rows.Scan(&p.Date, &p.Count); err != nil {
			return d, err
		}
		d.DailyCalls = append(d.DailyCalls, p)
	}
	sort.Slice(d.DailyCalls, func(i, j int) bool { return d.DailyCalls[i].Date < d.DailyCalls[j].Date })
	return d, rows.Err()
}

func (s *Store) ModelStats(ctx context.Context, days int) (ModelStats, error) {
	if days != 7 && days != 30 {
		return ModelStats{}, fmt.Errorf("days must be 7 or 30")
	}
	start := time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx, `SELECT day,model,calls,input_tokens+cache_read_tokens+cache_write_tokens,output_tokens,credits
		FROM daily_model_stats WHERE day>=? ORDER BY model,day`, start)
	if err != nil {
		return ModelStats{}, err
	}
	defer rows.Close()
	result := ModelStats{Models: make([]string, 0), Daily: map[string][]ModelPoint{}}
	seen := map[string]bool{}
	for rows.Next() {
		var model string
		var p ModelPoint
		if err := rows.Scan(&p.Date, &model, &p.Calls, &p.InputTokens, &p.OutputTokens, &p.Credits); err != nil {
			return result, err
		}
		if !seen[model] {
			seen[model] = true
			result.Models = append(result.Models, model)
		}
		result.Daily[model] = append(result.Daily[model], p)
	}
	return result, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }
