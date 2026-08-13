package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
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

func TestBulkAccountImportAcceptsLinesAndReportsDuplicates(t *testing.T) {
	_, mux, store := testService(t)
	cookie := login(t, mux)
	body := []byte(`{"keys":"# comment\nsk-bulk-one\n\nsk-bulk-two\nsk-bulk-one\n"}`)
	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/accounts/bulk", bytes.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://example.test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result bulkAccountResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || result.Created != 2 || result.Duplicates != 0 || result.Failed != 0 {
		t.Fatalf("bulk result=%#v", result)
	}
	accounts, err := store.Accounts(context.Background())
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}
	if strings.Contains(rec.Body.String(), "sk-bulk-one") || strings.Contains(rec.Body.String(), "sk-bulk-two") {
		t.Fatalf("bulk response leaked plaintext key: %s", rec.Body.String())
	}

	duplicate := httptest.NewRequest(http.MethodPost, "http://example.test/api/accounts/bulk", strings.NewReader(`{"keys":["sk-bulk-one","sk-bulk-three"]}`))
	duplicate.AddCookie(cookie)
	duplicate.Header.Set("Origin", "http://example.test")
	duplicate.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, duplicate)
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || result.Created != 1 || result.Duplicates != 1 {
		t.Fatalf("duplicate result=%#v", result)
	}
}

func TestBulkAccountImportAcceptsMultipartFile(t *testing.T) {
	_, mux, store := testService(t)
	cookie := login(t, mux)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "keys.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("sk-file-one\n# ignored\nsk-file-two\n"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/accounts/bulk", &body)
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://example.test")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("multipart status=%d body=%s", rec.Code, rec.Body.String())
	}
	accounts, err := store.Accounts(context.Background())
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
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

func postReload(t *testing.T, handler http.Handler, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/accounts/reload", strings.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://example.test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestReloadSelectedAccountsAndStream(t *testing.T) {
	service, mux, store := testService(t)
	cookie := login(t, mux)
	first, err := store.CreateAccount(context.Background(), config.AccountKey{APIKey: "sk-selected-first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateAccount(context.Background(), config.AccountKey{APIKey: "sk-selected-second"})
	if err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"account_ids":[%d,%d]}`, second.ID, second.ID)
	rec := postReload(t, mux, cookie, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("selected reload=%d %s", rec.Code, rec.Body.String())
	}
	service.Wait()
	service.reloadMu.Lock()
	progress := service.progressLocked()
	service.reloadMu.Unlock()
	if progress.Running || progress.Total != 1 || progress.Done != 1 {
		t.Fatalf("selected progress=%+v", progress)
	}
	unchanged, err := store.Account(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.BalanceAt != nil {
		t.Fatalf("unselected account was refreshed: %+v", unchanged)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.test/api/accounts/reload/stream", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total":1`) || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("reload stream=%d %s", rec.Code, rec.Body.String())
	}
}

func TestReloadRequestValidationAndCompatibility(t *testing.T) {
	service, mux, store := testService(t)
	cookie := login(t, mux)
	for _, key := range []string{"sk-reload-first", "sk-reload-second"} {
		if _, err := store.CreateAccount(context.Background(), config.AccountKey{APIKey: key}); err != nil {
			t.Fatal(err)
		}
	}

	for name, body := range map[string]string{
		"empty IDs":       `{"account_ids":[]}`,
		"null IDs":        `{"account_ids":null}`,
		"non-positive ID": `{"account_ids":[0]}`,
		"missing ID":      `{"account_ids":[999999]}`,
		"wrong type":      `{"account_ids":"1"}`,
		"unknown field":   `{"ids":[1]}`,
		"trailing JSON":   `{} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := postReload(t, mux, cookie, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("reload=%d %s", rec.Code, rec.Body.String())
			}
		})
	}

	rec := postReload(t, mux, cookie, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("full reload=%d %s", rec.Code, rec.Body.String())
	}
	service.Wait()
	service.reloadMu.Lock()
	progress := service.progressLocked()
	service.reloadMu.Unlock()
	if progress.Total != 2 || progress.Done != 2 {
		t.Fatalf("full progress=%+v", progress)
	}

	rec = postReload(t, mux, cookie, `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("object full reload=%d %s", rec.Code, rec.Body.String())
	}
	service.Wait()
}

func TestReloadRejectsConcurrentTask(t *testing.T) {
	service, mux, _ := testService(t)
	cookie := login(t, mux)
	service.reloadMu.Lock()
	service.reload.running = true
	service.reloadMu.Unlock()

	rec := postReload(t, mux, cookie, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already running") {
		t.Fatalf("concurrent reload=%d %s", rec.Code, rec.Body.String())
	}
}

func TestManualReloadRunsWhileAutomaticRefreshIsActive(t *testing.T) {
	service, mux, store := testService(t)
	cookie := login(t, mux)
	account, err := store.CreateAccount(context.Background(), config.AccountKey{APIKey: "sk-manual-during-auto"})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		writeJSON(w, http.StatusOK, map[string]any{
			"totalBalance":              10,
			"hasActivePaidSubscription": true,
		})
	}))
	defer upstreamServer.Close()
	service.cfg.Upstream.BaseURL = upstreamServer.URL
	if err := service.startAutomaticRefreshAll(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("automatic refresh did not start")
	}

	rec := postReload(t, mux, cookie, fmt.Sprintf(`{"account_ids":[%d]}`, account.ID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("manual reload during automatic refresh=%d %s", rec.Code, rec.Body.String())
	}
	close(release)
	service.Wait()
	service.reloadMu.Lock()
	progress := service.progressLocked()
	service.reloadMu.Unlock()
	if progress.Running || progress.Total != 1 || progress.Done != 1 {
		t.Fatalf("manual progress=%+v", progress)
	}
	service.autoRefreshMu.Lock()
	autoRefreshing := service.autoRefreshing
	service.autoRefreshMu.Unlock()
	if autoRefreshing {
		t.Fatal("automatic refresh remained active")
	}
}
