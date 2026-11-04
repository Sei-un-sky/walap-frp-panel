package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

const maxBody = 2 << 20

type Profile struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Config    string    `json:"config"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Runtime struct {
	Running      bool     `json:"running"`
	AdminEnabled bool     `json:"adminEnabled"`
	Connected    bool     `json:"connected"`
	PID          int      `json:"pid,omitempty"`
	Error        string   `json:"error,omitempty"`
	Tunnels      []Tunnel `json:"tunnels"`
	InBytes      uint64   `json:"inBytes"`
	OutBytes     uint64   `json:"outBytes"`
}

type Tunnel struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

type profileView struct {
	Profile
	Runtime Runtime `json:"runtime"`
}

type manager struct {
	mu       sync.Mutex
	profiles map[string]Profile
	process  map[string]*exec.Cmd
	dataDir  string
	frpc     string
}

func newManager(dataDir, frpc string) *manager {
	return &manager{profiles: map[string]Profile{}, process: map[string]*exec.Cmd{}, dataDir: dataDir, frpc: frpc}
}

func (m *manager) load() error {
	if err := os.MkdirAll(m.dataDir, 0700); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(m.dataDir, "profiles.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []Profile
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	for _, p := range list {
		if validID(p.ID) {
			m.profiles[p.ID] = p
		}
	}
	return nil
}

func (m *manager) persistLocked() error {
	list := make([]Profile, 0, len(m.profiles))
	for _, p := range m.profiles {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt.After(list[j].UpdatedAt) })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(m.dataDir, "profiles.json.tmp")
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(m.dataDir, "profiles.json"))
}

func validateConfig(config string) error {
	if strings.TrimSpace(config) == "" {
		return errors.New("TOML 配置不能为空")
	}
	if len(config) > maxBody {
		return errors.New("配置不能超过 2 MiB")
	}
	var data map[string]any
	if err := toml.Unmarshal([]byte(config), &data); err != nil {
		return fmt.Errorf("TOML 格式错误: %w", err)
	}
	return nil
}

func (m *manager) save(p Profile) (Profile, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 80 {
		return p, errors.New("名称长度须为 1 到 80 个字符")
	}
	if err := validateConfig(p.Config); err != nil {
		return p, err
	}
	if p.ID == "" {
		var err error
		p.ID, err = makeID()
		if err != nil {
			return p, err
		}
	}
	if !validID(p.ID) {
		return p, errors.New("无效的配置 ID")
	}
	p.Config = strings.TrimSpace(p.Config) + "\n"
	p.UpdatedAt = time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.process[p.ID] != nil {
		return p, errors.New("请先停止此 frpc 实例，再修改配置")
	}
	old, existed := m.profiles[p.ID]
	if existed {
		backupDir := filepath.Join(m.dataDir, "backups")
		if err := os.MkdirAll(backupDir, 0700); err != nil {
			return p, err
		}
		backup := filepath.Join(backupDir, p.ID+"."+time.Now().Format("20060102-150405.000000000")+".toml")
		if err := os.WriteFile(backup, []byte(old.Config), 0600); err != nil {
			return p, err
		}
	}
	m.profiles[p.ID] = p
	if err := m.persistLocked(); err != nil {
		if existed {
			m.profiles[p.ID] = old
		} else {
			delete(m.profiles, p.ID)
		}
		return p, err
	}
	return p, nil
}

func (m *manager) delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.process[id] != nil {
		return errors.New("请先停止此 frpc 实例")
	}
	p, ok := m.profiles[id]
	if !ok {
		return os.ErrNotExist
	}
	delete(m.profiles, id)
	if err := m.persistLocked(); err != nil {
		m.profiles[id] = p
		return err
	}
	return nil
}

func (m *manager) start(id string) error {
	m.mu.Lock()
	p, ok := m.profiles[id]
	if !ok {
		m.mu.Unlock()
		return os.ErrNotExist
	}
	if m.process[id] != nil {
		m.mu.Unlock()
		return errors.New("frpc 已在运行")
	}
	bin, err := exec.LookPath(m.frpc)
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("找不到 frpc 程序: %w", err)
	}
	configDir := filepath.Join(m.dataDir, "configs")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		m.mu.Unlock()
		return err
	}
	path := filepath.Join(configDir, id+".toml")
	if _, err := os.Stat(path); err == nil {
		backup := path + "." + time.Now().Format("20060102-150405") + ".bak"
		if err := os.Rename(path, backup); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(p.Config), 0600); err != nil {
		m.mu.Unlock()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		m.mu.Unlock()
		return err
	}
	cmd := exec.Command(bin, "-c", path)
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.process[id] = cmd
	m.mu.Unlock()
	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		if m.process[id] == cmd {
			delete(m.process, id)
		}
		m.mu.Unlock()
		if err != nil {
			log.Printf("frpc %s exited: %v", id, err)
		}
	}()
	return nil
}

func (m *manager) stop(id string) error {
	m.mu.Lock()
	cmd := m.process[id]
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return errors.New("frpc 未运行")
	}
	return cmd.Process.Kill()
}

func (m *manager) snapshot() []profileView {
	m.mu.Lock()
	profiles := make([]Profile, 0, len(m.profiles))
	for _, p := range m.profiles {
		profiles = append(profiles, p)
	}
	m.mu.Unlock()
	result := make([]profileView, 0, len(profiles))
	for _, p := range profiles {
		m.mu.Lock()
		cmd := m.process[p.ID]
		m.mu.Unlock()
		rt := Runtime{Running: cmd != nil, Tunnels: []Tunnel{}}
		if cmd != nil && cmd.Process != nil {
			rt.PID = cmd.Process.Pid
		}
		admin := readAdmin(p.Config)
		if admin != nil {
			rt.AdminEnabled = true
			client := http.Client{Timeout: time.Second}
			base := "http://" + net.JoinHostPort(admin.addr, strconv.Itoa(admin.port))
			for endpoint, kind := range map[string]string{"/api/status": "status", "/metrics": "metrics"} {
				req, _ := http.NewRequest(http.MethodGet, base+endpoint, nil)
				if admin.user != "" || admin.password != "" {
					req.SetBasicAuth(admin.user, admin.password)
				}
				resp, err := client.Do(req)
				if err != nil {
					rt.Error = "无法连接 frpc 管理接口"
					continue
				}
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					rt.Error = "frpc 管理接口返回异常状态"
					resp.Body.Close()
					continue
				}
				if kind == "status" {
					var body struct {
						Proxies []struct {
							Name   string `json:"name"`
							Type   string `json:"type"`
							Status string `json:"status"`
						} `json:"proxies"`
					}
					if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) == nil {
						rt.Connected = true
						for _, proxy := range body.Proxies {
							rt.Tunnels = append(rt.Tunnels, Tunnel{proxy.Name, proxy.Type, proxy.Status})
						}
					} else {
						rt.Error = "无法解析 frpc 隧道状态"
					}
				} else {
					rt.InBytes, rt.OutBytes = parseMetrics(resp.Body)
				}
				resp.Body.Close()
			}
		}
		result = append(result, profileView{p, rt})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	return result
}

type adminConfig struct {
	addr           string
	port           int
	user, password string
}

func readAdmin(config string) *adminConfig {
	var data struct {
		WebServer struct {
			Addr     string `toml:"addr"`
			Port     int    `toml:"port"`
			User     string `toml:"user"`
			Password string `toml:"password"`
		} `toml:"webServer"`
	}
	if toml.Unmarshal([]byte(config), &data) != nil || data.WebServer.Port < 1 || data.WebServer.Port > 65535 {
		return nil
	}
	addr := data.WebServer.Addr
	if addr == "" {
		addr = "127.0.0.1"
	}
	return &adminConfig{addr, data.WebServer.Port, data.WebServer.User, data.WebServer.Password}
}

func parseMetrics(body io.Reader) (uint64, uint64) {
	data, err := io.ReadAll(io.LimitReader(body, 2<<20))
	if err != nil {
		return 0, 0
	}
	var inBytes, outBytes uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.ToLower(fields[0])
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || value < 0 || !strings.Contains(name, "byte") {
			continue
		}
		if strings.Contains(name, "in") || strings.Contains(name, "recv") {
			inBytes += uint64(value)
		}
		if strings.Contains(name, "out") || strings.Contains(name, "send") {
			outBytes += uint64(value)
		}
	}
	return inBytes, outBytes
}

type webHandler struct {
	manager            *manager
	username, password string
	sessions           map[string]time.Time
	sessionMu          sync.Mutex
}

func (h *webHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w)
			return
		}
		name := filepath.Base(r.URL.Path)
		if name != "app.css" && name != "app.js" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join("web", name))
		return
	}
	if r.URL.Path == "/login.html" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w)
			return
		}
		http.ServeFile(w, r, "web/login.html")
		return
	}
	if r.URL.Path == "/api/login" && r.Method == http.MethodPost {
		h.login(w, r)
		return
	}
	if r.URL.Path == "/api/logout" && r.Method == http.MethodPost {
		if cookie, err := r.Cookie("frp_session"); err == nil {
			h.sessionMu.Lock()
			delete(h.sessions, cookie.Value)
			h.sessionMu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: "frp_session", Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.authorized(r) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusUnauthorized, "请先登录")
		} else {
			http.ServeFile(w, r, "web/login.html")
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.api(w, r)
		return
	}
	if r.URL.Path == "/" {
		http.ServeFile(w, r, "web/index.html")
		return
	}
	http.NotFound(w, r)
}

func (h *webHandler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if decodeJSON(w, r, &credentials) != nil || subtle.ConstantTimeCompare([]byte(credentials.Username), []byte(h.username)) != 1 || subtle.ConstantTimeCompare([]byte(credentials.Password), []byte(h.password)) != 1 {
		writeError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "无法创建登录会话")
		return
	}
	value := hex.EncodeToString(token[:])
	h.sessionMu.Lock()
	now := time.Now()
	for session, expiry := range h.sessions {
		if now.After(expiry) {
			delete(h.sessions, session)
		}
	}
	h.sessions[value] = now.Add(24 * time.Hour)
	h.sessionMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "frp_session", Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 86400})
	w.WriteHeader(http.StatusNoContent)
}

func (h *webHandler) authorized(r *http.Request) bool {
	cookie, err := r.Cookie("frp_session")
	if err != nil {
		return false
	}
	if len(cookie.Value) != 64 {
		return false
	}
	if _, err := hex.DecodeString(cookie.Value); err != nil {
		return false
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	expiry, ok := h.sessions[cookie.Value]
	if !ok || time.Now().After(expiry) {
		delete(h.sessions, cookie.Value)
		return false
	}
	return true
}

func (h *webHandler) api(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/me" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"username": h.username})
		return
	}
	if r.URL.Path == "/api/profiles" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, h.manager.snapshot())
		case http.MethodPost:
			var p Profile
			if err := decodeJSON(w, r, &p); err != nil {
				writeError(w, 400, err.Error())
				return
			}
			saved, err := h.manager.save(p)
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
			writeJSON(w, 201, profileView{saved, Runtime{Tunnels: []Tunnel{}}})
		default:
			methodNotAllowed(w)
		}
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/profiles/"), "/"), "/")
	if len(parts) < 1 || !validID(parts[0]) {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.manager.mu.Lock()
			p, ok := h.manager.profiles[id]
			h.manager.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, 200, profileView{p, h.manager.snapshotByID(id)})
		case http.MethodPut:
			var p Profile
			if err := decodeJSON(w, r, &p); err != nil {
				writeError(w, 400, err.Error())
				return
			}
			p.ID = id
			saved, err := h.manager.save(p)
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
			writeJSON(w, 200, profileView{saved, Runtime{Tunnels: []Tunnel{}}})
		case http.MethodDelete:
			if err := h.manager.delete(id); err != nil {
				writeError(w, 400, err.Error())
				return
			}
			w.WriteHeader(204)
		default:
			methodNotAllowed(w)
		}
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var err error
	if parts[1] == "start" {
		err = h.manager.start(id)
	} else if parts[1] == "stop" {
		err = h.manager.stop(id)
	} else {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (m *manager) snapshotByID(id string) Runtime {
	for _, item := range m.snapshot() {
		if item.ID == id {
			return item.Runtime
		}
	}
	return Runtime{Tunnels: []Tunnel{}}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("请求 JSON 格式无效")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func methodNotAllowed(w http.ResponseWriter) { writeError(w, 405, "请求方法不支持") }
func makeID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func validID(id string) bool {
	if len(id) != 24 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func main() {
	addr := os.Getenv("FRP_MANAGER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	user := os.Getenv("FRP_MANAGER_USER")
	password := os.Getenv("FRP_MANAGER_PASSWORD")
	if user == "" || password == "" {
		log.Fatal("必须设置 FRP_MANAGER_USER 和 FRP_MANAGER_PASSWORD")
	}
	dataDir := os.Getenv("FRP_MANAGER_DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}
	frpc := os.Getenv("FRPC_BINARY")
	if frpc == "" {
		frpc = "frpc"
	}
	m := newManager(dataDir, frpc)
	if err := m.load(); err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Addr: addr, Handler: &webHandler{manager: m, username: user, password: password, sessions: map[string]time.Time{}}, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("FRP manager listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
