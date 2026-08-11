package pool

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"todo2api/internal/config"
	"todo2api/internal/upstream"
)

// Account is one pooled todofor.ai API key + its upstream client.
type Account struct {
	Client    *upstream.Client
	ProjectID string
	Agent     upstream.AgentSettings
	EdgeTools upstream.FilteredEdgeTools // discovered once, forwarded per request
	inflight  int64
}

func (a *Account) Acquire() { atomic.AddInt64(&a.inflight, 1) }
func (a *Account) Release() { atomic.AddInt64(&a.inflight, -1) }

type Pool struct {
	accounts      []*Account
	strategy      string
	rr            uint64
	mu            sync.Mutex
	models        []upstream.ModelInfo
	modelByID     map[string]upstream.ModelInfo
	modelByRunner map[string]upstream.ModelInfo
	publicIDByID  map[string]string
	warnings      []error
}

func New(cfg *config.Config) (*Pool, error) {
	p := &Pool{strategy: cfg.Pool.Strategy}
	var catalogs [][]upstream.ModelInfo
	for index, k := range cfg.Pool.Keys {
		cli := upstream.New(cfg.Upstream.BaseURL, k.APIKey)
		models, discoveryErr := cli.Models(context.Background())
		if discoveryErr != nil {
			p.warnings = append(p.warnings, fmt.Errorf("discover models for account %d: %w", index+1, discoveryErr))
		} else {
			catalogs = append(catalogs, models)
		}
		pid := k.ProjectID
		if pid == "" {
			id, err := cli.FirstProject(context.Background())
			if err != nil {
				return nil, err
			}
			pid = id
		}
		var agent upstream.AgentSettings
		var err error
		if k.AgentID == "" {
			agent, err = cli.FirstAgent(context.Background())
		} else {
			agent, err = cli.Agent(context.Background(), k.AgentID)
		}
		if err != nil {
			return nil, err
		}
		acc := &Account{Client: cli, ProjectID: pid, Agent: agent}

		if cfg.Edge.Enabled {
			edgeID := cfg.Edge.ID()
			if edgeID == "" {
				id, err := cli.FirstOnlineEdge(context.Background())
				if err != nil {
					return nil, fmt.Errorf("edge enabled but %w", err)
				}
				edgeID = id
			}
			tools, err := cli.EdgeTools(context.Background(), edgeID, cfg.Edge.AllowTools)
			if err != nil {
				return nil, err
			}
			acc.EdgeTools = tools
		}
		p.accounts = append(p.accounts, acc)
	}
	if len(catalogs) == len(p.accounts) {
		p.setModels(commonModels(catalogs))
	} else {
		p.setModels(nil)
	}
	return p, nil
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
	if i < 0 || i >= len(p.accounts) {
		return nil
	}
	return p.accounts[i]
}

func (p *Pool) IndexOf(a *Account) int {
	for i, x := range p.accounts {
		if x == a {
			return i
		}
	}
	return -1
}

// Pick selects an account by the configured strategy.
func (p *Pool) Pick() *Account {
	if len(p.accounts) == 0 {
		return nil
	}
	switch p.strategy {
	case "least_busy":
		return p.leastBusy()
	default:
		return p.roundRobin()
	}
}

func (p *Pool) roundRobin() *Account {
	n := atomic.AddUint64(&p.rr, 1)
	return p.accounts[(n-1)%uint64(len(p.accounts))]
}

func (p *Pool) leastBusy() *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	best := p.accounts[0]
	for _, a := range p.accounts[1:] {
		if atomic.LoadInt64(&a.inflight) < atomic.LoadInt64(&best.inflight) {
			best = a
		}
	}
	return best
}
