package pool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"todo2api/internal/config"
	"todo2api/internal/upstream"
)

var ErrInitializationInProgress = errors.New("account initialization is already in progress")

// Account is one pooled todofor.ai API key + its upstream client.
type Account struct {
	ID            int64
	Client        *upstream.Client
	ProjectID     string
	Agent         upstream.AgentSettings
	EdgeTools     upstream.FilteredEdgeTools // discovered once, forwarded per request
	stateMu       sync.RWMutex
	inflight      int64
	cooldownUntil atomic.Int64
	disabled      atomic.Bool // permanent removal (e.g. exhausted balance)
	removed       atomic.Bool // soft-delete from key-file hot reload
	initing       atomic.Bool
	initialized   atomic.Bool
	removeMu      sync.Mutex
	key           config.AccountKey
	ready         atomic.Bool
}

// AccountRuntime is an immutable per-request view of an account. Callers keep
// this snapshot for the whole upstream operation so a concurrent reconcile can
// atomically install new credentials without mixing old and new state.
type AccountRuntime struct {
	Client    *upstream.Client
	ProjectID string
	Agent     upstream.AgentSettings
	EdgeTools upstream.FilteredEdgeTools
}

func newAccount(baseURL string, key config.AccountKey) *Account {
	return &Account{
		ID: key.ID, Client: upstream.New(baseURL, key.APIKey), key: key,
	}
}

func (a *Account) Runtime() AccountRuntime {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return AccountRuntime{
		Client: a.Client, ProjectID: a.ProjectID, Agent: a.Agent, EdgeTools: a.EdgeTools,
	}
}

func (a *Account) DatabaseID() int64 {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.ID
}

func (a *Account) keySnapshot() config.AccountKey {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.key
}

func (a *Account) initializationSnapshot() (AccountRuntime, config.AccountKey) {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return AccountRuntime{
		Client: a.Client, ProjectID: a.ProjectID, Agent: a.Agent, EdgeTools: a.EdgeTools,
	}, a.key
}

func (a *Account) applyInitialization(expected config.AccountKey, projectID string, agent upstream.AgentSettings, edgeTools upstream.FilteredEdgeTools) bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.key != expected {
		return false
	}
	a.ProjectID = projectID
	a.Agent = agent
	a.EdgeTools = edgeTools
	a.initialized.Store(true)
	return true
}

func (a *Account) setKey(key config.AccountKey) {
	a.stateMu.Lock()
	a.ID = key.ID
	a.key = key
	a.stateMu.Unlock()
}

func (a *Account) installCandidate(candidate *Account, key config.AccountKey) {
	runtime := candidate.Runtime()
	a.stateMu.Lock()
	a.ID = key.ID
	a.Client = runtime.Client
	a.ProjectID = runtime.ProjectID
	a.Agent = runtime.Agent
	a.EdgeTools = runtime.EdgeTools
	a.key = key
	a.stateMu.Unlock()
	a.initialized.Store(candidate.initialized.Load())
}

func (a *Account) Acquire() { atomic.AddInt64(&a.inflight, 1) }
func (a *Account) Release() { atomic.AddInt64(&a.inflight, -1) }

// CoolDown temporarily removes an unhealthy account from new-conversation
// selection. Existing sessions can still address it directly through At.
func (a *Account) CoolDown(duration time.Duration) {
	until := time.Now().Add(duration).UnixNano()
	for {
		current := a.cooldownUntil.Load()
		if current >= until || a.cooldownUntil.CompareAndSwap(current, until) {
			return
		}
	}
}

// ClearCooldown restores new-traffic eligibility after a successful health check.
func (a *Account) ClearCooldown() { a.cooldownUntil.Store(0) }

func (a *Account) available(now int64) bool {
	return !a.disabled.Load() && !a.removed.Load() && a.cooldownUntil.Load() <= now
}

// Removed reports whether the account was soft-deleted by a key-file reload.
// Soft-removed accounts stay addressable by index for in-flight sessions but
// are excluded from Pick for new traffic.
func (a *Account) Removed() bool { return a.removed.Load() }

// APIKey returns the upstream API key for this account.
func (a *Account) APIKey() string { return a.keySnapshot().APIKey }

func (a *Account) claimInit() bool {
	if a.initialized.Load() || a.removed.Load() || a.disabled.Load() {
		return false
	}
	return a.initing.CompareAndSwap(false, true)
}

func (a *Account) releaseInit() {
	a.initing.Store(false)
}

type Pool struct {
	accounts      []*Account
	configured    []*Account
	strategy      string
	rr            uint64
	mu            sync.Mutex // serializes Warm progress and ReloadKeys planning
	reconcileMu   sync.Mutex
	readyMu       sync.RWMutex
	models        []upstream.ModelInfo
	modelByID     map[string]upstream.ModelInfo
	modelByRunner map[string]upstream.ModelInfo
	publicIDByID  map[string]string
	warnings      []error
	cfg           *config.Config
	repo          AccountRepository
	warmStart     int
}

func (p *Pool) SetRepository(repo AccountRepository) {
	p.mu.Lock()
	p.repo = repo
	p.mu.Unlock()
}

type AccountRepository interface {
	SetEnabled(context.Context, int64, bool, string) error
	DeleteAccount(context.Context, int64) error
	SetHealthError(context.Context, int64, string, string) error
}

// ReloadStats summarizes a key-set hot reload.
type ReloadStats struct {
	Added      int
	Removed    int
	Restored   int
	Failed     int
	Ready      int
	Configured int
}

const (
	bootstrapAccounts = 4
	maxWarmWorkers    = 2
	maxWarmAttempts   = 3
	warmRetryDelay    = 500 * time.Millisecond
)

type accountInitResult struct {
	account      *Account
	models       []upstream.ModelInfo
	discoveryErr error
	err          error
}

func New(cfg *config.Config, repositories ...AccountRepository) (*Pool, error) {
	p := &Pool{strategy: cfg.Pool.Strategy, cfg: cfg}
	if len(repositories) > 0 {
		p.repo = repositories[0]
	}
	for _, key := range cfg.Pool.Keys {
		account := newAccount(cfg.Upstream.BaseURL, key)
		if key.ID != 0 && !key.Enabled {
			account.disabled.Store(true)
		}
		p.configured = append(p.configured, account)
	}
	activeConfigured := 0
	for _, account := range p.configured {
		if !account.disabled.Load() {
			activeConfigured++
		}
	}
	if activeConfigured == 0 {
		p.setModels(nil)
		return p, nil
	}
	var catalogs [][]upstream.ModelInfo
	var firstAccountErr error
	for p.warmStart < len(p.configured) && p.Len() == 0 {
		end := min(p.warmStart+bootstrapAccounts, len(p.configured))
		results := p.initializeBatch(context.Background(), p.warmStart, end, true, 1)
		for offset, result := range results {
			index := p.warmStart + offset
			if result.account != nil && result.account.disabled.Load() {
				continue
			}
			if result.err != nil {
				if firstAccountErr == nil {
					firstAccountErr = result.err
				}
				p.warnings = append(p.warnings, fmt.Errorf("account %d skipped: %w", index+1, result.err))
				continue
			}
			p.addReady(result.account)
			if result.discoveryErr != nil {
				p.warnings = append(p.warnings, fmt.Errorf("discover models for account %d: %w", index+1, result.discoveryErr))
			} else {
				catalogs = append(catalogs, result.models)
			}
		}
		p.warmStart = end
	}
	if p.Len() == 0 {
		if p.repo != nil {
			p.setModels(nil)
			return p, nil
		}
		if firstAccountErr != nil {
			return nil, fmt.Errorf("no usable accounts out of %d configured: %w", len(p.configured), firstAccountErr)
		}
		return nil, fmt.Errorf("no usable accounts out of %d configured", len(p.configured))
	}
	if len(catalogs) == p.Len() {
		p.setModels(commonModels(catalogs))
	} else {
		p.setModels(nil)
	}
	return p, nil
}

func (p *Pool) initializeBatch(ctx context.Context, start, end int, discoverModels bool, attempts int) []accountInitResult {
	results := make([]accountInitResult, end-start)
	var wg sync.WaitGroup
	for index := start; index < end; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			account := p.configured[index]
			if account.disabled.Load() {
				results[index-start] = accountInitResult{account: account}
				return
			}
			results[index-start] = initializeAccountWithRetry(
				ctx, p.cfg, account, discoverModels, attempts,
			)
		}(index)
	}
	wg.Wait()
	return results
}

func initializeAccountWithRetry(
	ctx context.Context,
	cfg *config.Config,
	account *Account,
	discoverModels bool,
	attempts int,
) accountInitResult {
	var result accountInitResult
	for attempt := 1; attempt <= attempts; attempt++ {
		result = initializeAccount(ctx, cfg, account, discoverModels)
		if result.err == nil || attempt == attempts {
			return result
		}
		delay := time.Duration(attempt) * warmRetryDelay
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return accountInitResult{err: ctx.Err()}
		}
	}
	return result
}

func initializeAccount(ctx context.Context, cfg *config.Config, account *Account, discoverModels bool) accountInitResult {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	runtime, key := account.initializationSnapshot()
	if runtime.Client == nil {
		return accountInitResult{account: account, err: fmt.Errorf("account client is unavailable")}
	}
	var (
		models       []upstream.ModelInfo
		discoveryErr error
		projectID    = key.ProjectID
		projectErr   error
		agent        upstream.AgentSettings
		agentErr     error
		wg           sync.WaitGroup
	)
	if discoverModels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			models, discoveryErr = runtime.Client.Models(ctx)
		}()
	}
	if projectID == "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			projectID, projectErr = runtime.Client.FirstProject(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if key.AgentID == "" {
			agent, agentErr = runtime.Client.FirstAgent(ctx)
		} else {
			agent, agentErr = runtime.Client.Agent(ctx, key.AgentID)
		}
	}()
	wg.Wait()

	if projectErr != nil {
		return accountInitResult{err: fmt.Errorf("find project: %w", projectErr)}
	}
	if agentErr != nil {
		return accountInitResult{err: fmt.Errorf("load agent: %w", agentErr)}
	}

	if cfg.Edge.Enabled {
		edgeID := cfg.Edge.ID()
		if edgeID == "" {
			id, err := runtime.Client.FirstOnlineEdge(ctx)
			if err != nil {
				return accountInitResult{err: fmt.Errorf("find online edge: %w", err)}
			}
			edgeID = id
		}
		tools, err := runtime.Client.EdgeTools(ctx, edgeID, cfg.Edge.AllowTools)
		if err != nil {
			return accountInitResult{err: fmt.Errorf("load edge tools: %w", err)}
		}
		runtime.EdgeTools = tools
	}
	if !account.applyInitialization(key, projectID, agent, runtime.EdgeTools) {
		return accountInitResult{account: account, err: fmt.Errorf("account configuration changed during initialization")}
	}
	return accountInitResult{account: account, models: models, discoveryErr: discoveryErr}
}

// Models returns the models common to every account. IDs are the shortest
// unambiguous public aliases. An incomplete discovery yields no dynamic list.
func (p *Pool) Models() []upstream.ModelInfo {
	return append([]upstream.ModelInfo(nil), p.models...)
}

// Model finds model metadata by public alias, full upstream ID, or runner ID.
func (p *Pool) Model(id string) (upstream.ModelInfo, bool) {
	if model, ok := p.modelByID[id]; ok {
		return model, true
	}
	model, ok := p.modelByRunner[id]
	return model, ok
}

// ResolveModel converts any discovered model ID into AgentSettings format.
func (p *Pool) ResolveModel(id string) string {
	if model, ok := p.Model(id); ok {
		return upstream.RunnerModelID(model.ID)
	}
	return id
}

// PublicModelID returns the short public alias for a discovered model.
func (p *Pool) PublicModelID(id string) (string, bool) {
	model, ok := p.Model(id)
	if !ok {
		return "", false
	}
	publicID, ok := p.publicIDByID[model.ID]
	return publicID, ok
}

func (p *Pool) Warnings() []error {
	return append([]error(nil), p.warnings...)
}

// Len returns the number of ready accounts that are not permanently disabled
// or soft-removed. Cooldown accounts still count.
func (p *Pool) Len() int {
	p.readyMu.RLock()
	defer p.readyMu.RUnlock()
	n := 0
	for _, account := range p.accounts {
		if account != nil && !account.disabled.Load() && !account.removed.Load() {
			n++
		}
	}
	return n
}

// Configured returns the total number of unique non-removed, non-disabled
// configured accounts.
func (p *Pool) Configured() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, account := range p.configured {
		if !account.disabled.Load() && !account.removed.Load() {
			n++
		}
	}
	return n
}

// Warm initializes the remaining configured accounts in bounded background
// batches. onBatch receives cumulative ready/skipped/processed counts.
func (p *Pool) Warm(ctx context.Context, onBatch func(ready, skipped, configured int)) {
	skipped := 0
	for {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		start := p.warmStart
		if start >= len(p.configured) {
			p.mu.Unlock()
			return
		}
		end := min(start+maxWarmWorkers, len(p.configured))
		batch := append([]*Account(nil), p.configured[start:end]...)
		p.warmStart = end
		processed := end
		p.mu.Unlock()

		for _, account := range batch {
			if !account.claimInit() {
				continue
			}
			result := initializeAccountWithRetry(ctx, p.cfg, account, false, maxWarmAttempts)
			if result.err != nil {
				account.releaseInit()
				skipped++
				continue
			}
			p.addReady(result.account)
		}
		if onBatch != nil {
			onBatch(p.Len(), skipped, processed)
		}
	}
}

// ReloadKeys reconciles the pool with the desired key set. Existing account
// indices are preserved: removed keys are soft-deleted (excluded from Pick),
// and reappearing keys are restored in place when possible.
func (p *Pool) ReloadKeys(ctx context.Context, keys []config.AccountKey) (ReloadStats, error) {
	p.reconcileMu.Lock()
	defer p.reconcileMu.Unlock()

	desired, desiredOrder := normalizeDesiredKeys(keys)
	type changedAccount struct {
		account *Account
		key     config.AccountKey
	}

	p.mu.Lock()
	byKey := make(map[string]*Account, len(p.configured))
	activeKeys := make(map[string]struct{}, len(p.configured))
	for _, account := range p.configured {
		key := account.keySnapshot()
		identity := keyIdentity(key)
		byKey[identity] = account
		if !account.removed.Load() {
			activeKeys[identity] = struct{}{}
		}
	}

	var stats ReloadStats
	var needInit []*Account
	var changed []changedAccount

	for key := range activeKeys {
		if _, ok := desired[key]; ok {
			continue
		}
		account := byKey[key]
		account.removed.Store(true)
		stats.Removed++
	}

	for _, key := range desiredOrder {
		identity := keyIdentity(key)
		if account, ok := byKey[identity]; ok {
			wasRemoved := account.removed.Load()
			wasDisabled := account.disabled.Load()
			oldKey := account.keySnapshot()
			configurationChanged := oldKey.APIKey != key.APIKey || oldKey.ProjectID != key.ProjectID || oldKey.AgentID != key.AgentID
			if configurationChanged {
				// Stop new selection until a complete runtime can be installed.
				account.removed.Store(true)
				account.disabled.Store(key.ID != 0 && !key.Enabled)
				changed = append(changed, changedAccount{account: account, key: key})
			} else {
				account.setKey(key)
				account.removed.Store(false)
				account.disabled.Store(key.ID != 0 && !key.Enabled)
			}
			if wasRemoved || (wasDisabled && key.Enabled) {
				stats.Restored++
			}
			if !configurationChanged && key.Enabled && !account.initialized.Load() {
				needInit = append(needInit, account)
			}
			continue
		}
		account := newAccount(p.cfg.Upstream.BaseURL, key)
		if key.ID != 0 && !key.Enabled {
			account.disabled.Store(true)
		}
		p.configured = append(p.configured, account)
		byKey[identity] = account
		stats.Added++
		if !account.disabled.Load() {
			needInit = append(needInit, account)
		}
	}
	stats.Configured = 0
	for _, key := range desiredOrder {
		if key.ID == 0 || key.Enabled {
			stats.Configured++
		}
	}
	p.mu.Unlock()

	for _, update := range changed {
		update.account.removeMu.Lock()
		candidate := newAccount(p.cfg.Upstream.BaseURL, update.key)
		if update.key.ID != 0 && !update.key.Enabled {
			candidate.disabled.Store(true)
			update.account.installCandidate(candidate, update.key)
			update.account.removed.Store(false)
			update.account.removeMu.Unlock()
			continue
		}
		result := initializeAccountWithRetry(ctx, p.cfg, candidate, false, maxWarmAttempts)
		if result.err != nil {
			update.account.installCandidate(candidate, update.key)
			update.account.disabled.Store(true)
			update.account.removeMu.Unlock()
			stats.Failed++
			continue
		}
		update.account.installCandidate(candidate, update.key)
		update.account.disabled.Store(false)
		update.account.removed.Store(false)
		p.addReady(update.account)
		update.account.removeMu.Unlock()
	}

	for i, account := range needInit {
		if err := ctx.Err(); err != nil {
			for _, remaining := range needInit[i:] {
				if !remaining.initialized.Load() && !remaining.removed.Load() && !remaining.disabled.Load() {
					stats.Failed++
				}
			}
			stats.Ready = p.Len()
			return stats, err
		}
		if !account.claimInit() {
			continue
		}
		result := initializeAccountWithRetry(ctx, p.cfg, account, false, maxWarmAttempts)
		if result.err != nil {
			account.releaseInit()
			stats.Failed++
			continue
		}
		p.addReady(result.account)
	}
	stats.Ready = p.Len()
	return stats, nil
}

func keyIdentity(key config.AccountKey) string {
	if key.ID != 0 {
		return fmt.Sprintf("id:%d", key.ID)
	}
	return "key:" + key.APIKey
}

func normalizeDesiredKeys(keys []config.AccountKey) (map[string]config.AccountKey, []config.AccountKey) {
	desired := make(map[string]config.AccountKey, len(keys))
	order := make([]config.AccountKey, 0, len(keys))
	for _, key := range keys {
		key.APIKey = strings.TrimSpace(key.APIKey)
		if key.APIKey == "" {
			continue
		}
		identity := keyIdentity(key)
		if _, exists := desired[identity]; exists {
			continue
		}
		desired[identity] = key
		order = append(order, key)
	}
	return desired, order
}

func (p *Pool) addReady(account *Account) {
	account.releaseInit()
	if account.removed.Load() || account.disabled.Load() {
		return
	}
	if !account.ready.CompareAndSwap(false, true) {
		return
	}
	p.readyMu.Lock()
	p.accounts = append(p.accounts, account)
	p.readyMu.Unlock()
}

func (p *Pool) setModels(models []upstream.ModelInfo) {
	publicIDs := publicModelIDs(models)
	p.models = make([]upstream.ModelInfo, 0, len(models))
	p.modelByID = make(map[string]upstream.ModelInfo, len(models)*2)
	p.modelByRunner = make(map[string]upstream.ModelInfo, len(models))
	p.publicIDByID = make(map[string]string, len(models))
	for i, model := range models {
		publicID := publicIDs[i]
		advertised := model
		advertised.ID = publicID
		p.models = append(p.models, advertised)

		p.modelByID[publicID] = model
		p.modelByID[model.ID] = model
		p.modelByRunner[upstream.RunnerModelID(model.ID)] = model
		p.publicIDByID[model.ID] = publicID
	}
	sort.Slice(p.models, func(i, j int) bool { return p.models[i].ID < p.models[j].ID })
}

func publicModelIDs(models []upstream.ModelInfo) []string {
	ids := make([]string, len(models))
	canonicalOwners := make(map[string]int, len(models))
	for i, model := range models {
		ids[i] = shortModelID(model.ID)
		canonicalOwners[model.ID] = i
	}

	// A provider prefix is only kept when the short ID would be ambiguous or
	// would shadow another model's full ID.
	for {
		counts := make(map[string]int, len(ids))
		for _, id := range ids {
			counts[id]++
		}
		changed := false
		for i, id := range ids {
			owner, shadowsCanonical := canonicalOwners[id]
			if counts[id] > 1 || (shadowsCanonical && owner != i) {
				if ids[i] != models[i].ID {
					ids[i] = models[i].ID
					changed = true
				}
			}
		}
		if !changed {
			return ids
		}
	}
}

func shortModelID(id string) string {
	_, short, ok := strings.Cut(id, "/")
	if !ok || short == "" {
		return id
	}
	return short
}

func commonModels(catalogs [][]upstream.ModelInfo) []upstream.ModelInfo {
	if len(catalogs) == 0 {
		return nil
	}
	common := make(map[string]upstream.ModelInfo, len(catalogs[0]))
	for _, model := range catalogs[0] {
		if model.ID != "" {
			common[model.ID] = model
		}
	}
	for _, catalog := range catalogs[1:] {
		available := make(map[string]struct{}, len(catalog))
		for _, model := range catalog {
			available[model.ID] = struct{}{}
		}
		for id := range common {
			if _, ok := available[id]; !ok {
				delete(common, id)
			}
		}
	}
	models := make([]upstream.ModelInfo, 0, len(common))
	for _, model := range common {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

func (p *Pool) At(i int) *Account {
	p.readyMu.RLock()
	defer p.readyMu.RUnlock()
	if i < 0 || i >= len(p.accounts) {
		return nil
	}
	account := p.accounts[i]
	if account.disabled.Load() {
		return nil
	}
	return account
}

func (p *Pool) AccountByID(id int64) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, account := range p.configured {
		if account.DatabaseID() == id {
			return account
		}
	}
	return nil
}

func (p *Pool) IndexOf(a *Account) int {
	p.readyMu.RLock()
	defer p.readyMu.RUnlock()
	for i, x := range p.accounts {
		if x == a {
			return i
		}
	}
	return -1
}

// Remove deletes an account from its configured credential source and
// permanently excludes it from new-conversation selection. The stable slot is
// retained because active sessions persist account indexes.
func (p *Pool) Remove(a *Account) error {
	p.reconcileMu.Lock()
	defer p.reconcileMu.Unlock()

	p.readyMu.RLock()
	found := false
	for _, account := range p.accounts {
		if account == a {
			found = true
			break
		}
	}
	p.readyMu.RUnlock()
	if !found {
		return fmt.Errorf("account is not in the pool")
	}

	a.removeMu.Lock()
	defer a.removeMu.Unlock()
	if a.disabled.Load() {
		return nil
	}
	a.disabled.Store(true)
	id := a.DatabaseID()
	if p.repo == nil || id == 0 {
		// Compatibility for in-memory pools used by tests and embedders.
		keys := p.cfg.Pool.Keys[:0]
		for _, key := range p.cfg.Pool.Keys {
			if key.APIKey != a.APIKey() {
				keys = append(keys, key)
			}
		}
		p.cfg.Pool.Keys = keys
		return nil
	}
	if err := p.repo.SetEnabled(context.Background(), id, false, "exhausted"); err != nil {
		a.disabled.Store(false)
		return fmt.Errorf("persist account disable: %w", err)
	}
	return nil
}

func (p *Pool) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	return p.SetEnabledReason(ctx, id, enabled, "manual")
}

func (p *Pool) SetEnabledReason(ctx context.Context, id int64, enabled bool, reason string) error {
	p.reconcileMu.Lock()
	defer p.reconcileMu.Unlock()

	account := p.AccountByID(id)
	if account == nil {
		return fmt.Errorf("account not found")
	}
	if p.repo == nil {
		return fmt.Errorf("account repository is unavailable")
	}
	account.removeMu.Lock()
	defer account.removeMu.Unlock()

	if !enabled {
		if err := p.repo.SetEnabled(ctx, id, false, reason); err != nil {
			return err
		}
		account.disabled.Store(true)
		return nil
	}

	if !account.initialized.Load() {
		if !account.initing.CompareAndSwap(false, true) {
			return ErrInitializationInProgress
		}
		defer account.releaseInit()
		result := initializeAccountWithRetry(ctx, p.cfg, account, false, maxWarmAttempts)
		if result.err != nil {
			account.disabled.Store(true)
			_ = p.repo.SetEnabled(context.Background(), id, false, reason)
			_ = p.repo.SetHealthError(context.Background(), id, "error", result.err.Error())
			return result.err
		}
	}
	if err := p.repo.SetEnabled(ctx, id, true, reason); err != nil {
		account.disabled.Store(true)
		return err
	}
	account.disabled.Store(false)
	account.removed.Store(false)
	p.addReady(account)
	return nil
}

func (p *Pool) DeleteByID(ctx context.Context, id int64) error {
	p.reconcileMu.Lock()
	defer p.reconcileMu.Unlock()

	account := p.AccountByID(id)
	if account == nil {
		return fmt.Errorf("account not found")
	}
	if p.repo == nil {
		return fmt.Errorf("account repository is unavailable")
	}
	account.removeMu.Lock()
	defer account.removeMu.Unlock()
	if err := p.repo.DeleteAccount(ctx, id); err != nil {
		return err
	}
	account.disabled.Store(true)
	account.removed.Store(true)
	return nil
}

func (p *Pool) Refresh(ctx context.Context, id int64) error {
	p.reconcileMu.Lock()
	defer p.reconcileMu.Unlock()

	account := p.AccountByID(id)
	if account == nil {
		return fmt.Errorf("account not found")
	}
	account.removeMu.Lock()
	defer account.removeMu.Unlock()
	if !account.initing.CompareAndSwap(false, true) {
		return ErrInitializationInProgress
	}
	defer account.releaseInit()
	result := initializeAccountWithRetry(ctx, p.cfg, account, false, 1)
	if result.err != nil {
		return result.err
	}
	if !account.ready.Load() {
		p.addReady(account)
	}
	return nil
}

func (p *Pool) MarkInvalid(ctx context.Context, account *Account, cause error) {
	if p.repo == nil || account == nil || account.DatabaseID() == 0 {
		return
	}
	_ = p.repo.SetHealthError(ctx, account.DatabaseID(), "invalid", cause.Error())
}

// Pick selects an account by the configured strategy.
func (p *Pool) Pick() *Account {
	return p.PickExcept(nil)
}

// PickExcept selects an available account that is not in excluded. Rotating
// the scan start also distributes least-busy traffic across idle accounts.
func (p *Pool) PickExcept(excluded map[*Account]struct{}) *Account {
	p.readyMu.RLock()
	defer p.readyMu.RUnlock()
	if len(p.accounts) == 0 {
		return nil
	}
	start := int((atomic.AddUint64(&p.rr, 1) - 1) % uint64(len(p.accounts)))
	switch p.strategy {
	case "least_busy":
		return p.leastBusy(start, excluded)
	default:
		return p.roundRobin(start, excluded)
	}
}

func (p *Pool) roundRobin(start int, excluded map[*Account]struct{}) *Account {
	now := time.Now().UnixNano()
	for offset := range p.accounts {
		account := p.accounts[(start+offset)%len(p.accounts)]
		if _, skip := excluded[account]; !skip && account.available(now) {
			return account
		}
	}
	return nil
}

func (p *Pool) leastBusy(start int, excluded map[*Account]struct{}) *Account {
	now := time.Now().UnixNano()
	var best *Account
	for offset := range p.accounts {
		account := p.accounts[(start+offset)%len(p.accounts)]
		if _, skip := excluded[account]; skip || !account.available(now) {
			continue
		}
		if best == nil || atomic.LoadInt64(&account.inflight) < atomic.LoadInt64(&best.inflight) {
			best = account
		}
	}
	return best
}
