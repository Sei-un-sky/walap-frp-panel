package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestLoginAndProtectedAPI(t *testing.T) {
	h := &webHandler{manager: newManager(t.TempDir(), "frpc"), username: "admin", password: "secret", sessions: map[string]time.Time{}}
	api := httptest.NewRecorder()
	h.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/profiles", nil))
	if api.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API status = %d", api.Code)
	}
	login := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"secret"}`))
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
