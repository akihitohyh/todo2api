package pool

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"todo2api/internal/config"
	"todo2api/internal/upstream"
)

// Account is one pooled todofor.ai API key + its upstream client.
type Account struct {
	Client        *upstream.Client
	ProjectID     string
	Agent         upstream.AgentSettings
	EdgeTools     upstream.FilteredEdgeTools // discovered once, forwarded per request
	inflight      int64
	cooldownUntil atomic.Int64
	disabled      atomic.Bool // permanent removal (e.g. exhausted balance)
	removed       atomic.Bool // soft-delete from key-file hot reload
	initing       atomic.Bool
	removeMu      sync.Mutex
	key           config.AccountKey
	ready         atomic.Bool
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

func (a *Account) available(now int64) bool {
	return !a.disabled.Load() && !a.removed.Load() && a.cooldownUntil.Load() <= now
}

// Removed reports whether the account was soft-deleted by a key-file reload.
// Soft-removed accounts stay addressable by index for in-flight sessions but
// are excluded from Pick for new traffic.
func (a *Account) Removed() bool { return a.removed.Load() }

// APIKey returns the upstream API key for this account.
func (a *Account) APIKey() string { return a.key.APIKey }

func (a *Account) claimInit() bool {
	if a.ready.Load() || a.removed.Load() || a.disabled.Load() {
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
	readyMu       sync.RWMutex
	models        []upstream.ModelInfo
	modelByID     map[string]upstream.ModelInfo
	modelByRunner map[string]upstream.ModelInfo
	publicIDByID  map[string]string
	warnings      []error
	cfg           *config.Config
	warmStart     int
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

func New(cfg *config.Config) (*Pool, error) {
	p := &Pool{strategy: cfg.Pool.Strategy, cfg: cfg}
	for _, key := range cfg.Pool.Keys {
		p.configured = append(p.configured, &Account{
			Client: upstream.New(cfg.Upstream.BaseURL, key.APIKey), key: key,
		})
	}
	var catalogs [][]upstream.ModelInfo
	var firstAccountErr error
	for p.warmStart < len(p.configured) && p.Len() == 0 {
		end := min(p.warmStart+bootstrapAccounts, len(p.configured))
		results := p.initializeBatch(context.Background(), p.warmStart, end, true, 1)
		for offset, result := range results {
			index := p.warmStart + offset
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
			results[index-start] = initializeAccountWithRetry(
				ctx, p.cfg, p.configured[index], discoverModels, attempts,
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

	var (
		models       []upstream.ModelInfo
		discoveryErr error
		projectID    = account.key.ProjectID
		projectErr   error
		agent        upstream.AgentSettings
		agentErr     error
		wg           sync.WaitGroup
	)
	if discoverModels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			models, discoveryErr = account.Client.Models(ctx)
		}()
	}
	if projectID == "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			projectID, projectErr = account.Client.FirstProject(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if account.key.AgentID == "" {
			agent, agentErr = account.Client.FirstAgent(ctx)
		} else {
			agent, agentErr = account.Client.Agent(ctx, account.key.AgentID)
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
			id, err := account.Client.FirstOnlineEdge(ctx)
			if err != nil {
				return accountInitResult{err: fmt.Errorf("find online edge: %w", err)}
			}
			edgeID = id
		}
		tools, err := account.Client.EdgeTools(ctx, edgeID, cfg.Edge.AllowTools)
		if err != nil {
			return accountInitResult{err: fmt.Errorf("load edge tools: %w", err)}
		}
		account.EdgeTools = tools
	}
	account.ProjectID = projectID
	account.Agent = agent
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
	desired, desiredOrder := normalizeDesiredKeys(keys)

	p.mu.Lock()
	byKey := make(map[string]*Account, len(p.configured))
	activeKeys := make(map[string]struct{}, len(p.configured))
	for _, account := range p.configured {
		byKey[account.key.APIKey] = account
		if !account.removed.Load() && !account.disabled.Load() {
			activeKeys[account.key.APIKey] = struct{}{}
		}
	}

	var stats ReloadStats
	var needInit []*Account

	for key := range activeKeys {
		if _, ok := desired[key]; ok {
			continue
		}
		account := byKey[key]
		account.removed.Store(true)
		stats.Removed++
	}

	for _, key := range desiredOrder {
		if account, ok := byKey[key.APIKey]; ok {
			wasOut := account.removed.Load() || account.disabled.Load()
			if wasOut {
				account.removed.Store(false)
				account.disabled.Store(false)
				account.key = key
				stats.Restored++
				if !account.ready.Load() {
					needInit = append(needInit, account)
				}
			}
			continue
		}
		account := &Account{
			Client: upstream.New(p.cfg.Upstream.BaseURL, key.APIKey),
			key:    key,
		}
		p.configured = append(p.configured, account)
		byKey[key.APIKey] = account
		stats.Added++
		needInit = append(needInit, account)
	}
	stats.Configured = 0
	for _, account := range p.configured {
		if !account.disabled.Load() && !account.removed.Load() {
			stats.Configured++
		}
	}
	p.mu.Unlock()

	for i, account := range needInit {
		if err := ctx.Err(); err != nil {
			for _, remaining := range needInit[i:] {
				if !remaining.ready.Load() && !remaining.removed.Load() && !remaining.disabled.Load() {
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

func normalizeDesiredKeys(keys []config.AccountKey) (map[string]config.AccountKey, []config.AccountKey) {
	desired := make(map[string]config.AccountKey, len(keys))
	order := make([]config.AccountKey, 0, len(keys))
	for _, key := range keys {
		key.APIKey = strings.TrimSpace(key.APIKey)
		if key.APIKey == "" {
			continue
		}
		if _, exists := desired[key.APIKey]; exists {
			continue
		}
		desired[key.APIKey] = key
		order = append(order, key)
	}
	return desired, order
}

func (p *Pool) addReady(account *Account) {
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
	if p.cfg == nil {
		a.disabled.Store(false)
		return fmt.Errorf("pool configuration is unavailable")
	}
	if err := p.cfg.RemovePoolKey(a.key.APIKey); err != nil {
		a.disabled.Store(false)
		return fmt.Errorf("persist account removal: %w", err)
	}
	return nil
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
