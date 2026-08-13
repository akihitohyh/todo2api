package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"todo2api/internal/config"
	"todo2api/internal/pool"
	"todo2api/internal/storage"
)

func testService(t *testing.T) (*Service, *http.ServeMux, *storage.Store) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	data := `
storage: {path: data/test.db, master_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
web: {admin_username: admin, admin_password: secret, session_ttl: 12h}
pool: {strategy: round_robin, keys: []}
models: {default: "openai:openai/test"}
upstream: {base_url: "http://127.0.0.1:1/api/v1", poll_timeout: 1s}
`
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), cfg, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := store.PoolKeys(context.Background())
	cfg.Pool.Keys = keys
	p, err := pool.New(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := New(cfg, store, p, ctx)
	mux := http.NewServeMux()
	service.Register(mux)
	t.Cleanup(func() {
		cancel()
		service.Wait()
		store.Close()
	})
	return service, mux, store
}

func TestProxyHeadersRequireExplicitTrust(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://todo2api.local/", nil)
	r.RemoteAddr = "192.0.2.20:4321"
	r.Header.Set("X-Forwarded-For", "198.51.100.9")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "public.example")

	_, trustedNetwork, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteIP(r, nil); got != "192.0.2.20" {
		t.Fatalf("untrusted remote IP = %q", got)
	}
	if got := requestScheme(r, false); got != "http" {
		t.Fatalf("untrusted scheme = %q", got)
	}
	if got := requestHost(r, false); got != "todo2api.local" {
		t.Fatalf("untrusted host = %q", got)
	}
	if got := remoteIP(r, []*net.IPNet{trustedNetwork}); got != "198.51.100.9" {
		t.Fatalf("trusted remote IP = %q", got)
	}
	if got := requestScheme(r, true); got != "https" {
		t.Fatalf("trusted scheme = %q", got)
	}
	if got := requestHost(r, true); got != "public.example" {
		t.Fatalf("trusted host = %q", got)
	}
}

func TestRemoteIPSkipsTrustedProxyChainFromRight(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://todo2api.local/", nil)
	r.RemoteAddr = "192.0.2.20:4321"
	r.Header.Set("X-Forwarded-For", "203.0.113.8, 192.0.2.30")
	_, trustedNetwork, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteIP(r, []*net.IPNet{trustedNetwork}); got != "203.0.113.8" {
		t.Fatalf("remote IP = %q", got)
	}
}

func login(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "secret"})
	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies=%v", cookies)
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie=%+v", cookies[0])
	}
	return cookies[0]
}

func TestAuthenticationAndRegistrationDisabled(t *testing.T) {
	_, mux, _ := testService(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.test/api/accounts", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", rec.Code)
	}
	cookie := login(t, mux)
	req = httptest.NewRequest(http.MethodGet, "http://example.test/api/auth/check", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "true") {
		t.Fatalf("check=%d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "http://example.test/api/register/start", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("register status=%d", rec.Code)
	}
}

func TestAccountCRUDAndOriginProtection(t *testing.T) {
	_, mux, store := testService(t)
	cookie := login(t, mux)
	body := []byte(`{"api_key":"sk-admin-test","project_id":"project"}`)
	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/accounts", bytes.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://evil.test")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("origin status=%d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "http://example.test/api/accounts", bytes.NewReader(body))
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing origin status=%d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "http://example.test/api/accounts", bytes.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://example.test")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	accounts, err := store.Accounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}
	id := accounts[0].ID
	req = httptest.NewRequest(http.MethodPatch, "http://example.test/api/accounts/"+strconv.FormatInt(id, 10), strings.NewReader(`{"enabled":false}`))
	req.AddCookie(cookie)
	req.Header.Set("Referer", "http://example.test/accounts")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("disable=%d %s", rec.Code, rec.Body.String())
	}
	account, _ := store.Account(context.Background(), id)
	if account.Enabled {
		t.Fatal("account remained enabled")
	}
	req = httptest.NewRequest(http.MethodDelete, "http://example.test/api/accounts/"+strconv.FormatInt(id, 10), nil)
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://example.test")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete=%d %s", rec.Code, rec.Body.String())
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	_, mux, store := testService(t)
	token, err := store.CreateAdminSession(context.Background(), -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.test/api/accounts", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired status=%d", rec.Code)
	}
}
