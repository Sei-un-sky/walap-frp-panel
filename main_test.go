package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateConfig(t *testing.T) {
	if err := validateConfig("serverAddr = \"relay.example\"\nserverPort = 7000\n"); err != nil {
		t.Fatalf("valid TOML rejected: %v", err)
	}
	if err := validateConfig("serverPort = ["); err == nil {
		t.Fatal("invalid TOML accepted")
	}
	if err := validateConfig("  \n"); err == nil {
		t.Fatal("empty TOML accepted")
	}
}

func TestParseMetrics(t *testing.T) {
	input := "frp_proxy_in_bytes_total 1024\nfrp_proxy_out_bytes_total 2048\nother_metric 88\n"
	inBytes, outBytes := parseMetrics(strings.NewReader(input))
	if inBytes != 1024 || outBytes != 2048 {
		t.Fatalf("got (%d, %d), want (1024, 2048)", inBytes, outBytes)
	}
}

func TestReadFRPCStatusGroupedByType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tcp":[{"name":"ssh","type":"tcp","status":"running","err":""}],"http":[{"name":"site","type":"http","status":"start error","err":"local port unavailable"}]}`))
	}))
	defer server.Close()

	tunnels, connected, message := readFRPCStatus(server.Client(), server.URL, &adminConfig{})
	if !connected || message != "" {
		t.Fatalf("status connected=%v message=%q", connected, message)
	}
	if len(tunnels) != 2 || tunnels[0].Name != "site" || tunnels[0].Status != "start error" || tunnels[1].Name != "ssh" || tunnels[1].Status != "running" {
		t.Fatalf("unexpected tunnels: %#v", tunnels)
	}
}

func TestLoginAndProtectedAPI(t *testing.T) {
	dir := t.TempDir()
	users := newUserStore(dir)
	if _, err := users.add("admin", "admin", "initial-password", false); err != nil {
		t.Fatal(err)
	}
	h := &webHandler{manager: newManager(dir, "frpc"), users: users, sessions: map[string]session{}}
	api := httptest.NewRecorder()
	h.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/profiles", nil))
	if api.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API status = %d", api.Code)
	}
	login := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"initial-password"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(login, req)
	if login.Code != http.StatusNoContent {
		t.Fatalf("login status = %d", login.Code)
	}
	cookie := login.Result().Cookies()[0]
	api = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/profiles", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(api, req)
	if api.Code != http.StatusOK {
		t.Fatalf("authenticated API status = %d", api.Code)
	}

	logout := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(logout, req)
	api = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/profiles", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(api, req)
	if api.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out API status = %d", api.Code)
	}
}

func TestTemporaryPasswordRequiresChange(t *testing.T) {
	dir := t.TempDir()
	users := newUserStore(dir)
	if _, err := users.add("admin", "admin", "temporary-pass", true); err != nil {
		t.Fatal(err)
	}
	h := &webHandler{manager: newManager(dir, "frpc"), users: users, sessions: map[string]session{}}
	login := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"temporary-pass"}`))
	h.ServeHTTP(login, req)
	if login.Code != http.StatusNoContent {
		t.Fatalf("login status = %d", login.Code)
	}
	cookie := login.Result().Cookies()[0]
	blocked := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/profiles", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(blocked, req)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("temporary password API status = %d", blocked.Code)
	}
	changed := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/password", strings.NewReader(`{"new":"a-new-password"}`))
	req.AddCookie(cookie)
	h.ServeHTTP(changed, req)
	if changed.Code != http.StatusNoContent {
		t.Fatalf("password change status = %d, body=%s", changed.Code, changed.Body.String())
	}
	if _, ok := users.authenticate("admin", "a-new-password"); !ok {
		t.Fatal("new password does not authenticate")
	}
}

func TestUserStoreAccountsAndLastAdmin(t *testing.T) {
	store := newUserStore(t.TempDir())
	if _, err := store.add("Root.Admin", "admin", "temporary-pass", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.authenticate("root.admin", "temporary-pass"); !ok {
		t.Fatal("case-normalized username did not authenticate")
	}
	if _, err := store.add("ops", "user", "another-password", false); err != nil {
		t.Fatal(err)
	}
	if err := store.remove("Root.Admin"); err == nil {
		t.Fatal("removed the last administrator")
	}
	reloaded := newUserStore(filepath.Dir(store.path))
	if err := reloaded.load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.get("root.admin"); !ok {
		t.Fatal("administrator was not persisted")
	}
}
