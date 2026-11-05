package main

import (
	"net/http"
	"net/http/httptest"
	"os"
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

func TestReadProxyTraffic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/traffic/ssh" {
			t.Errorf("request path = %q", r.URL.Path)
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "dash" || password != "secret" {
			t.Errorf("dashboard basic auth not sent")
		}
		_, _ = w.Write([]byte(`{"name":"ssh","trafficIn":[1000,234],"trafficOut":[5000,678]}`))
	}))
	defer server.Close()
	tunnels := []Tunnel{{Name: "ssh", Type: "tcp"}}
	readProxyTraffic(server.Client(), server.URL, "dash", "secret", tunnels)
	if !tunnels[0].TrafficSet || tunnels[0].InBytes != 1234 || tunnels[0].OutBytes != 5678 {
		t.Fatalf("unexpected tunnel traffic: %#v", tunnels[0])
	}
}

func TestReadProxyTrafficReportsAuthenticationAndMissingProxy(t *testing.T) {
	for _, test := range []struct {
		status int
		want   string
	}{{http.StatusUnauthorized, "frps Dashboard 认证失败"}, {http.StatusNotFound, "Dashboard API 返回 HTTP 404：请确认这里填写的是 frps Dashboard 根地址（不是 frpc 管理页），且反向代理保留 /api 路径"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(test.status) }))
		tunnels := []Tunnel{{Name: "ssh"}}
		got := readProxyTraffic(server.Client(), server.URL, "", "", tunnels)
		server.Close()
		if got != test.want {
			t.Errorf("status %d error=%q, want %q", test.status, got, test.want)
		}
	}
}

func TestAutostartPersistsAndStartsSelectedProfiles(t *testing.T) {
	dir := t.TempDir()
	m := newManager(dir, "frpc")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	frpc := filepath.Join(dir, "bin", "frpc")
	if err := os.WriteFile(frpc, []byte("#!/bin/sh\nsleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	m.frpc = frpc
	p, err := m.save(Profile{Name: "auto", Config: "serverAddr = \"127.0.0.1\"\nserverPort = 7000"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.setAutostart(p.ID, true); err != nil {
		t.Fatal(err)
	}
	reloaded := newManager(dir, frpc)
	if err := reloaded.load(); err != nil {
		t.Fatal(err)
	}
	if !reloaded.profiles[p.ID].Autostart {
		t.Fatal("autostart flag was not persisted")
	}
	reloaded.startAutostart()
	if reloaded.process[p.ID] == nil {
		t.Fatal("autostart profile did not start")
	}
	if err := reloaded.stop(p.ID); err != nil {
		t.Fatal(err)
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
