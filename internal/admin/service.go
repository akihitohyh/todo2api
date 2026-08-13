package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"todo2api/internal/config"
	"todo2api/internal/pool"
	"todo2api/internal/storage"
	"todo2api/internal/upstream"
)

const cookieName = "todo2api-admin"

type Service struct {
	cfg            *config.Config
	store          *storage.Store
	pool           *pool.Pool
	ctx            context.Context
	hub            *eventHub
	loginMu        sync.Mutex
	loginAttempts  map[string]loginAttempt
	loginCleanup   time.Time
	trustedProxies []*net.IPNet
	reloadMu       sync.Mutex
	reload         reloadState
	accountLocks   sync.Map
	workers        sync.WaitGroup
}

type loginAttempt struct {
	failures     int
	blockedUntil time.Time
	lastSeen     time.Time
}

type reloadState struct {
	running   bool
	total     int
	done      int
	exhausted int
	invalid   int
	subs      map[chan reloadProgress]struct{}
}

type reloadProgress struct {
	Running   bool `json:"running"`
	Total     int  `json:"total"`
	Done      int  `json:"done"`
	Exhausted int  `json:"exhausted"`
	Invalid   int  `json:"invalid"`
}

type apiAccount struct {
	ID               int64      `json:"id"`
	Email            string     `json:"email"`
	Name             string     `json:"name"`
	APIKeyMasked     string     `json:"api_key_masked"`
	ProjectID        string     `json:"project_id,omitempty"`
	AgentID          string     `json:"agent_id,omitempty"`
	Balance          float64    `json:"balance"`
	BalanceUnlimited bool       `json:"balance_unlimited"`
	BalanceAt        *time.Time `json:"balance_at,omitempty"`
	Status           string     `json:"status"`
	Enabled          bool       `json:"enabled"`
	LastError        string     `json:"last_error,omitempty"`
}

func New(cfg *config.Config, store *storage.Store, p *pool.Pool, contexts ...context.Context) *Service {
	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	s := &Service{cfg: cfg, store: store, pool: p, ctx: ctx, hub: newEventHub(), loginAttempts: map[string]loginAttempt{}}
	for _, cidr := range cfg.Web.TrustedProxies {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil {
			s.trustedProxies = append(s.trustedProxies, network)
		}
	}
	s.reload.subs = map[chan reloadProgress]struct{}{}
	return s
}

func (s *Service) RecordCall(ctx context.Context, model string, usage storage.Usage, success bool) error {
	if err := s.store.RecordCall(ctx, model, usage, success); err != nil {
		return err
	}
	s.hub.publish("stats")
	return nil
}

func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/auth/login", s.sameOrigin(s.handleLogin))
	mux.HandleFunc("/api/auth/check", s.handleCheck)
	mux.HandleFunc("/api/auth/logout", s.requireAuth(s.sameOrigin(s.handleLogout)))
	mux.HandleFunc("/api/accounts", s.requireAuth(s.sameOrigin(s.handleAccounts)))
	mux.HandleFunc("/api/accounts/", s.requireAuth(s.sameOrigin(s.handleAccount)))
	mux.HandleFunc("/api/stats", s.requireAuth(s.handleStats))
	mux.HandleFunc("/api/stats/models", s.requireAuth(s.handleModelStats))
	mux.HandleFunc("/api/events", s.requireAuth(s.handleEvents))
}

func (s *Service) Run(ctx context.Context) {
	_ = s.startRefreshAll()
	ticker := time.NewTicker(5 * time.Minute)
	cleanup := time.NewTicker(time.Hour)
	defer ticker.Stop()
	defer cleanup.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.startRefreshAll()
		case <-cleanup.C:
			if err := s.store.CleanupSessions(ctx); err != nil {
				log.Printf("admin session cleanup: %v", err)
			}
		}
	}
}

// Wait blocks until all health-refresh workers have exited.
func (s *Service) Wait() { s.workers.Wait() }

func (s *Service) SetEnabled(ctx context.Context, id int64, enabled bool, reason string) error {
	if err := s.store.SetEnabled(ctx, id, enabled, reason); err != nil {
		return err
	}
	s.hub.publish("accounts")
	s.hub.publish("stats")
	return nil
}

func (s *Service) DeleteAccount(ctx context.Context, id int64) error {
	if err := s.store.DeleteAccount(ctx, id); err != nil {
		return err
	}
	s.hub.publish("accounts")
	s.hub.publish("stats")
	return nil
}

func (s *Service) SetHealthError(ctx context.Context, id int64, status, message string) error {
	if err := s.store.SetHealthError(ctx, id, status, message); err != nil {
		return err
	}
	s.hub.publish("accounts")
	s.hub.publish("stats")
	return nil
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	now := time.Now()
	ip := remoteIP(r, s.trustedProxies)
	s.loginMu.Lock()
	s.cleanupLoginAttemptsLocked(now)
	if _, exists := s.loginAttempts[ip]; !exists && len(s.loginAttempts) >= maxLoginAttemptEntries-1 {
		ip = loginOverflowKey
	}
	attempt := s.loginAttempts[ip]
	if now.Before(attempt.blockedUntil) {
		s.loginMu.Unlock()
		writeError(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}
	s.loginMu.Unlock()
	var req struct{ Username, Password string }
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	valid := subtle.ConstantTimeCompare([]byte(req.Username), []byte(s.cfg.Web.AdminUsername)) == 1 &&
		subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.cfg.Web.AdminPassword)) == 1
	if !valid {
		s.loginMu.Lock()
		attempt = s.loginAttempts[ip]
		attempt.failures++
		attempt.lastSeen = now
		if attempt.failures >= 5 {
			attempt.blockedUntil = now.Add(15 * time.Minute)
			attempt.failures = 0
		}
		s.loginAttempts[ip] = attempt
		s.loginMu.Unlock()
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	s.loginMu.Lock()
	delete(s.loginAttempts, ip)
	s.loginMu.Unlock()
	token, err := s.store.CreateAdminSession(r.Context(), s.cfg.Web.SessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create session failed")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: s.cfg.Web.SecureCookie, SameSite: http.SameSiteStrictMode, MaxAge: int(s.cfg.Web.SessionTTL.Seconds())})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (s *Service) handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ok, _ := s.authenticated(r)
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": ok})
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if cookie, err := r.Cookie(cookieName); err == nil {
		_ = s.store.DeleteAdminSession(r.Context(), cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.Web.SecureCookie, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		accounts, err := s.store.Accounts(r.Context())
		if err != nil {
			writeError(w, 500, "list accounts failed")
			return
		}
		result := make([]apiAccount, 0, len(accounts))
		for _, a := range accounts {
			result = append(result, toAPIAccount(a))
		}
		writeJSON(w, 200, result)
	case http.MethodPost:
		var req struct {
			APIKey    string `json:"api_key"`
			ProjectID string `json:"project_id"`
			AgentID   string `json:"agent_id"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.APIKey) == "" {
			writeError(w, 400, "api_key is required")
			return
		}
		a, err := s.store.CreateAccount(r.Context(), config.AccountKey{APIKey: req.APIKey, ProjectID: req.ProjectID, AgentID: req.AgentID})
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		if err := s.syncPool(r.Context()); err != nil {
			log.Printf("sync pool after add: %v", err)
			_ = s.store.SetHealthError(r.Context(), a.ID, "error", err.Error())
		}
		s.hub.publish("accounts")
		s.hub.publish("stats")
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			_, _ = s.refreshOne(s.ctx, a.ID)
		}()
		writeJSON(w, http.StatusCreated, toAPIAccount(a))
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (s *Service) handleAccount(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	if path == "reload" {
		s.handleReload(w, r)
		return
	}
	if path == "reload/stream" {
		s.handleReloadStream(w, r)
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeError(w, 404, "account not found")
		return
	}
	if len(parts) == 2 && parts[1] == "refresh" {
		if r.Method != http.MethodPost {
			writeError(w, 405, "method not allowed")
			return
		}
		a, err := s.refreshOne(r.Context(), id)
		if err != nil {
			s.hub.publish("stats")
			writeError(w, 502, err.Error())
			return
		}
		s.hub.publish("stats")
		writeJSON(w, 200, toAPIAccount(a))
		return
	}
	if len(parts) != 1 {
		writeError(w, 404, "not found")
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Enabled == nil {
			writeError(w, 400, "enabled is required")
			return
		}
		unlock := s.lockAccount(id)
		err := s.pool.SetEnabled(r.Context(), id, *req.Enabled)
		unlock()
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		s.hub.publish("accounts")
		a, _ := s.store.Account(r.Context(), id)
		writeJSON(w, 200, toAPIAccount(a))
	case http.MethodDelete:
		unlock := s.lockAccount(id)
		err := s.pool.DeleteByID(r.Context(), id)
		unlock()
		if err != nil {
			writeError(w, 404, err.Error())
			return
		}
		s.hub.publish("accounts")
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (s *Service) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	s.reloadMu.Lock()
	running := s.reload.running
	s.reloadMu.Unlock()
	if !running {
		if err := s.startRefreshAll(); err != nil {
			writeError(w, 500, "start health check failed")
			return
		}
	}
	writeJSON(w, 202, map[string]string{"status": "started"})
}

func (s *Service) startRefreshAll() error {
	accounts, err := s.store.Accounts(s.ctx)
	if err != nil {
		return err
	}
	s.reloadMu.Lock()
	if s.reload.running {
		s.reloadMu.Unlock()
		return nil
	}
	s.reload = reloadState{running: true, total: len(accounts), subs: s.reload.subs}
	s.reloadMu.Unlock()
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		s.refreshAccounts(s.ctx, accounts)
	}()
	return nil
}

func (s *Service) refreshAccounts(ctx context.Context, accounts []storage.Account) {
	jobs := make(chan int64)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				a, err := s.refreshOne(ctx, id)
				s.reloadMu.Lock()
				s.reload.done++
				if err != nil {
					s.reload.invalid++
				} else if a.DisabledReason == "exhausted" {
					s.reload.exhausted++
				}
				s.broadcastProgressLocked()
				s.reloadMu.Unlock()
			}
		}()
	}
loop:
	for _, a := range accounts {
		select {
		case jobs <- a.ID:
		case <-ctx.Done():
			break loop
		}
	}
	close(jobs)
	wg.Wait()
	s.reloadMu.Lock()
	s.reload.running = false
	s.broadcastProgressLocked()
	s.reloadMu.Unlock()
	s.hub.publish("accounts")
	s.hub.publish("stats")
}

func (s *Service) refreshOne(ctx context.Context, id int64) (storage.Account, error) {
	unlock := s.lockAccount(id)
	defer unlock()

	a, err := s.store.Account(ctx, id)
	if err != nil {
		return a, err
	}
	cli := upstream.New(s.cfg.Upstream.BaseURL, a.APIKey)
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	usage, err := cli.BillingUsage(checkCtx)
	if err != nil {
		status := "error"
		var httpErr *upstream.HTTPError
		if errors.As(err, &httpErr) && (httpErr.StatusCode == 401 || httpErr.StatusCode == 403) {
			status = "invalid"
			if account := s.pool.AccountByID(id); account != nil {
				account.CoolDown(time.Hour)
			}
		}
		if updateErr := s.store.UpdateHealth(ctx, id, storage.HealthUpdate{Status: status, Balance: a.Balance, BalanceUnlimited: a.BalanceUnlimited, SubscriptionTier: a.SubscriptionTier, Error: err.Error()}); updateErr != nil {
			return a, updateErr
		}
		s.hub.publish("accounts")
		s.hub.publish("stats")
		updated, _ := s.store.Account(ctx, id)
		return updated, err
	}
	exhausted := usage.TotalBalance <= 0 && !usage.HasActivePaidSubscription
	if usage.Session != nil && usage.Session.UsedPercent >= 100 {
		exhausted = true
	}
	if usage.Weekly != nil && usage.Weekly.UsedPercent >= 100 {
		exhausted = true
	}
	status := "active"
	reason := ""
	if exhausted {
		status = "exhausted"
		reason = "exhausted"
	}
	if err := s.store.UpdateHealth(ctx, id, storage.HealthUpdate{Status: status, Balance: usage.TotalBalance, BalanceUnlimited: false, SubscriptionTier: usage.Tier, Disable: exhausted, DisabledReason: reason}); err != nil {
		return a, err
	}
	current, err := s.store.Account(ctx, id)
	if err != nil {
		return a, err
	}
	if exhausted {
		if acc := s.pool.AccountByID(id); acc != nil {
			_ = s.pool.SetEnabledReason(ctx, id, false, "exhausted")
		}
	} else if current.Enabled {
		if account := s.pool.AccountByID(id); account != nil {
			account.ClearCooldown()
		}
		if err := s.pool.Refresh(ctx, id); err != nil {
			if errors.Is(err, pool.ErrInitializationInProgress) {
				s.hub.publish("accounts")
				s.hub.publish("stats")
				return s.store.Account(ctx, id)
			}
			_ = s.store.SetHealthError(ctx, id, "error", err.Error())
			s.hub.publish("accounts")
			s.hub.publish("stats")
			updated, _ := s.store.Account(ctx, id)
			return updated, err
		}
	}
	s.hub.publish("accounts")
	s.hub.publish("stats")
	return s.store.Account(ctx, id)
}

func (s *Service) lockAccount(id int64) func() {
	value, _ := s.accountLocks.LoadOrStore(id, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Service) syncPool(ctx context.Context) error {
	keys, err := s.store.PoolKeys(ctx)
	if err != nil {
		return err
	}
	_, err = s.pool.ReloadKeys(ctx, keys)
	return err
}

func (s *Service) handleReloadStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan reloadProgress, 8)
	s.reloadMu.Lock()
	s.reload.subs[ch] = struct{}{}
	current := s.progressLocked()
	s.reloadMu.Unlock()
	defer func() { s.reloadMu.Lock(); delete(s.reload.subs, ch); s.reloadMu.Unlock() }()
	sendProgress(w, current)
	flusher.Flush()
	if !current.Running && current.Done >= current.Total {
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	for {
		select {
		case p := <-ch:
			sendProgress(w, p)
			if !p.Running && p.Done >= p.Total {
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Service) progressLocked() reloadProgress {
	return reloadProgress{Running: s.reload.running, Total: s.reload.total, Done: s.reload.done, Exhausted: s.reload.exhausted, Invalid: s.reload.invalid}
}
func (s *Service) broadcastProgressLocked() {
	p := s.progressLocked()
	for ch := range s.reload.subs {
		select {
		case ch <- p:
		default:
		}
	}
}
func sendProgress(w http.ResponseWriter, p reloadProgress) {
	b, _ := json.Marshal(p)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func (s *Service) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	stats, err := s.store.Dashboard(r.Context())
	if err != nil {
		writeError(w, 500, "load stats failed")
		return
	}
	writeJSON(w, 200, stats)
}
func (s *Service) handleModelStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days == 0 {
		days = 30
	}
	stats, err := s.store.ModelStats(r.Context(), days)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, stats)
}
func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) { s.hub.serve(w, r, s.ctx) }

func (s *Service) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := s.authenticated(r)
		if err != nil || !ok {
			writeError(w, 401, "authentication required")
			return
		}
		next(w, r)
	}
}
func (s *Service) authenticated(r *http.Request) (bool, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false, nil
	}
	return s.store.ValidAdminSession(r.Context(), c.Value)
}
func (s *Service) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			origin := r.Header.Get("Origin")
			if origin == "" {
				origin = r.Referer()
			}
			u, err := url.Parse(origin)
			trustProxyHeaders := s.trustProxyHeaders(r)
			if err != nil || u.Scheme == "" || !strings.EqualFold(u.Host, requestHost(r, trustProxyHeaders)) || !strings.EqualFold(u.Scheme, requestScheme(r, trustProxyHeaders)) {
				writeError(w, 403, "same-origin request required")
				return
			}
		}
		next(w, r)
	}
}

func requestScheme(r *http.Request, trustProxyHeaders bool) string {
	if r.TLS != nil {
		return "https"
	}
	if trustProxyHeaders {
		proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
		if strings.EqualFold(proto, "http") || strings.EqualFold(proto, "https") {
			return strings.ToLower(proto)
		}
	}
	return "http"
}

func requestHost(r *http.Request, trustProxyHeaders bool) string {
	if trustProxyHeaders {
		if host := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); host != "" {
			return host
		}
	}
	return r.Host
}

func (s *Service) trustProxyHeaders(r *http.Request) bool {
	peer := peerIP(r.RemoteAddr)
	return peer != nil && ipInNetworks(peer, s.trustedProxies)
}

func remoteIP(r *http.Request, trustedProxies []*net.IPNet) string {
	peer := peerIP(r.RemoteAddr)
	if peer != nil && ipInNetworks(peer, trustedProxies) {
		chain := make([]net.IP, 0)
		for _, value := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
			if ip := net.ParseIP(strings.TrimSpace(value)); ip != nil {
				chain = append(chain, ip)
			}
		}
		for i := len(chain) - 1; i >= 0; i-- {
			if !ipInNetworks(chain[i], trustedProxies) {
				return chain[i].String()
			}
		}
		if len(chain) > 0 {
			return chain[0].String()
		}
		if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
			return ip.String()
		}
	}
	if peer != nil {
		return peer.String()
	}
	if ip := net.ParseIP(strings.TrimSpace(r.RemoteAddr)); ip != nil {
		return ip.String()
	}
	return r.RemoteAddr
}

func peerIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(strings.TrimSpace(remoteAddr))
}

func ipInNetworks(ip net.IP, networks []*net.IPNet) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

const (
	maxLoginAttemptEntries = 4096
	loginOverflowKey       = "__overflow__"
)

func (s *Service) cleanupLoginAttemptsLocked(now time.Time) {
	if now.Sub(s.loginCleanup) < time.Minute && len(s.loginAttempts) < maxLoginAttemptEntries {
		return
	}
	cutoff := now.Add(-30 * time.Minute)
	for ip, attempt := range s.loginAttempts {
		if attempt.lastSeen.Before(cutoff) && !now.Before(attempt.blockedUntil) {
			delete(s.loginAttempts, ip)
		}
	}
	s.loginCleanup = now
}

func toAPIAccount(a storage.Account) apiAccount {
	status := a.HealthStatus
	if !a.Enabled {
		if a.DisabledReason == "exhausted" {
			status = "exhausted"
		} else {
			status = "disabled"
		}
	}
	name := a.SubscriptionTier
	if name == "" {
		name = "API Key"
	}
	return apiAccount{ID: a.ID, Name: name, APIKeyMasked: a.APIKeyMasked, ProjectID: a.ProjectID, AgentID: a.AgentID, Balance: a.Balance, BalanceUnlimited: a.BalanceUnlimited, BalanceAt: a.BalanceAt, Status: status, Enabled: a.Enabled, LastError: a.LastError}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

type eventHub struct {
	mu          sync.Mutex
	subscribers map[chan string]struct{}
}

func newEventHub() *eventHub { return &eventHub{subscribers: map[chan string]struct{}{}} }
func (h *eventHub) publish(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}
func (h *eventHub) serve(w http.ResponseWriter, r *http.Request, shutdown context.Context) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan string, 8)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.subscribers, ch); h.mu.Unlock() }()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	for {
		select {
		case event := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", event)
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-shutdown.Done():
			return
		}
	}
}
