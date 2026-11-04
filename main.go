package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
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
const passwordIterations = 310000
const minPasswordLength = 8

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
	TrafficKnown bool     `json:"trafficKnown"`
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
	Error  string `json:"err,omitempty"`
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

type account struct {
	Username   string    `json:"username"`
	Role       string    `json:"role"`
	Salt       string    `json:"salt"`
	Hash       string    `json:"hash"`
	MustChange bool      `json:"mustChange"`
	CreatedAt  time.Time `json:"createdAt"`
}

type userStore struct {
	mu    sync.Mutex
	path  string
	users map[string]account
}

type session struct {
	Username string
	Expires  time.Time
}

type publicAccount struct {
	Username   string `json:"username"`
	Role       string `json:"role"`
	MustChange bool   `json:"mustChange"`
}

func newUserStore(dataDir string) *userStore {
	return &userStore{path: filepath.Join(dataDir, "users.json"), users: map[string]account{}}
}

func (s *userStore) load() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var users []account
	if err := json.Unmarshal(b, &users); err != nil {
		return err
	}
	for _, u := range users {
		if validUsername(u.Username) && (u.Role == "admin" || u.Role == "user") {
			s.users[u.Username] = u
		}
	}
	return nil
}

func (s *userStore) saveLocked() error {
	users := make([]account, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	b, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *userStore) add(username, role, password string, mustChange bool) (account, error) {
	username = strings.TrimSpace(strings.ToLower(username))
	if !validUsername(username) {
		return account{}, errors.New("用户名需为 3 到 32 位字母、数字、点、下划线或连字符")
	}
	if role != "admin" && role != "user" {
		return account{}, errors.New("角色必须是 admin 或 user")
	}
	if len(password) < minPasswordLength {
		return account{}, fmt.Errorf("密码至少需要 %d 个字符", minPasswordLength)
	}
	user, err := newAccount(username, role, password, mustChange)
	if err != nil {
		return account{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[username]; ok {
		return account{}, errors.New("用户名已存在")
	}
	s.users[username] = user
	if err := s.saveLocked(); err != nil {
		delete(s.users, username)
		return account{}, err
	}
	return user, nil
}

func (s *userStore) authenticate(username, password string) (account, bool) {
	s.mu.Lock()
	user, ok := s.users[strings.ToLower(strings.TrimSpace(username))]
	s.mu.Unlock()
	if !ok {
		return account{}, false
	}
	salt, err := hex.DecodeString(user.Salt)
	if err != nil {
		return account{}, false
	}
	expected, err := hex.DecodeString(user.Hash)
	if err != nil {
		return account{}, false
	}
	actual := passwordHash(password, salt)
	return user, subtle.ConstantTimeCompare(actual, expected) == 1
}

func (s *userStore) changePassword(username, oldPassword, newPassword string, skipOld bool) error {
	if len(newPassword) < minPasswordLength {
		return fmt.Errorf("新密码至少需要 %d 个字符", minPasswordLength)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[username]
	if !ok {
		return errors.New("账号不存在")
	}
	if !skipOld {
		salt, _ := hex.DecodeString(user.Salt)
		expected, _ := hex.DecodeString(user.Hash)
		if subtle.ConstantTimeCompare(passwordHash(oldPassword, salt), expected) != 1 {
			return errors.New("当前密码错误")
		}
	}
	updated, err := newAccount(user.Username, user.Role, newPassword, false)
	if err != nil {
		return err
	}
	updated.CreatedAt = user.CreatedAt
	s.users[username] = updated
	if err := s.saveLocked(); err != nil {
		s.users[username] = user
		return err
	}
	return nil
}

func (s *userStore) remove(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[username]
	if !ok {
		return os.ErrNotExist
	}
	if user.Role == "admin" {
		admins := 0
		for _, u := range s.users {
			if u.Role == "admin" {
				admins++
			}
		}
		if admins <= 1 {
			return errors.New("不能删除最后一个管理员")
		}
	}
	delete(s.users, username)
	if err := s.saveLocked(); err != nil {
		s.users[username] = user
		return err
	}
	return nil
}

func (s *userStore) resetPassword(username, password string) error {
	if len(password) < minPasswordLength {
		return fmt.Errorf("临时密码至少需要 %d 个字符", minPasswordLength)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[username]
	if !ok {
		return os.ErrNotExist
	}
	updated, err := newAccount(user.Username, user.Role, password, true)
	if err != nil {
		return err
	}
	updated.CreatedAt = user.CreatedAt
	s.users[username] = updated
	if err := s.saveLocked(); err != nil {
		s.users[username] = user
		return err
	}
	return nil
}

func (s *userStore) list() []publicAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]publicAccount, 0, len(s.users))
	for _, u := range s.users {
		result = append(result, publicAccount{u.Username, u.Role, u.MustChange})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Username < result[j].Username })
	return result
}

func (s *userStore) get(username string) (account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[username]
	return u, ok
}

func newAccount(username, role, password string, mustChange bool) (account, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return account{}, err
	}
	return account{Username: username, Role: role, Salt: hex.EncodeToString(salt), Hash: hex.EncodeToString(passwordHash(password, salt)), MustChange: mustChange, CreatedAt: time.Now().UTC()}, nil
}

func passwordHash(password string, salt []byte) []byte {
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write(append(append([]byte(nil), salt...), 0, 0, 0, 1))
	u := mac.Sum(nil)
	result := append([]byte(nil), u...)
	for i := 1; i < passwordIterations; i++ {
		mac = hmac.New(sha256.New, []byte(password))
		mac.Write(u)
		u = mac.Sum(nil)
		for j := range result {
			result[j] ^= u[j]
		}
	}
	return result
}

func validUsername(username string) bool {
	if len(username) < 3 || len(username) > 32 {
		return false
	}
	for _, r := range username {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func randomPassword() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "Frp!" + hex.EncodeToString(b[:]), nil
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
			rt.Tunnels, rt.Connected, rt.Error = readFRPCStatus(&client, base, admin)
		}
		result = append(result, profileView{p, rt})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	return result
}

func readFRPCStatus(client *http.Client, base string, admin *adminConfig) ([]Tunnel, bool, string) {
	req, err := http.NewRequest(http.MethodGet, base+"/api/status", nil)
	if err != nil {
		return nil, false, "无法创建 frpc 状态请求"
	}
	if admin.user != "" || admin.password != "" {
		req.SetBasicAuth(admin.user, admin.password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, "无法连接 frpc 管理接口"
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, "frpc 管理接口返回异常状态"
	}
	var grouped map[string][]Tunnel
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&grouped); err != nil {
		return nil, false, "无法解析 frpc 隧道状态"
	}
	tunnels := make([]Tunnel, 0)
	for proxyType, proxies := range grouped {
		for _, proxy := range proxies {
			if proxy.Type == "" {
				proxy.Type = proxyType
			}
			tunnels = append(tunnels, proxy)
		}
	}
	sort.Slice(tunnels, func(i, j int) bool { return tunnels[i].Name < tunnels[j].Name })
	return tunnels, true, ""
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
	manager   *manager
	users     *userStore
	sessions  map[string]session
	sessionMu sync.Mutex
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
		if name != "app.css" && name != "app.js" && name != "account.js" && name != "theme.js" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join("web", name))
		return
	}
	if r.URL.Path == "/login.html" || r.URL.Path == "/account.html" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w)
			return
		}
		page := "login.html"
		if r.URL.Path == "/account.html" {
			page = "account.html"
		}
		http.ServeFile(w, r, filepath.Join("web", page))
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
	user, authenticated := h.currentUser(r)
	if !authenticated {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusUnauthorized, "请先登录")
		} else {
			http.Redirect(w, r, "/login.html", http.StatusSeeOther)
		}
		return
	}
	if user.MustChange && r.URL.Path != "/account.html" && r.URL.Path != "/api/password" && r.URL.Path != "/api/me" && r.URL.Path != "/api/logout" {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusForbidden, "首次登录请先修改临时密码")
		} else {
			http.Redirect(w, r, "/account.html", http.StatusSeeOther)
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.api(w, r, user)
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
	if decodeJSON(w, r, &credentials) != nil {
		writeError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	user, ok := h.users.authenticate(credentials.Username, credentials.Password)
	if !ok {
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
	for token, sess := range h.sessions {
		if now.After(sess.Expires) {
			delete(h.sessions, token)
		}
	}
	h.sessions[value] = session{Username: user.Username, Expires: now.Add(24 * time.Hour)}
	h.sessionMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "frp_session", Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 86400})
	w.WriteHeader(http.StatusNoContent)
}

func (h *webHandler) currentUser(r *http.Request) (account, bool) {
	cookie, err := r.Cookie("frp_session")
	if err != nil {
		return account{}, false
	}
	if len(cookie.Value) != 64 {
		return account{}, false
	}
	if _, err := hex.DecodeString(cookie.Value); err != nil {
		return account{}, false
	}
	h.sessionMu.Lock()
	sess, ok := h.sessions[cookie.Value]
	if !ok || time.Now().After(sess.Expires) {
		delete(h.sessions, cookie.Value)
		h.sessionMu.Unlock()
		return account{}, false
	}
	h.sessionMu.Unlock()
	user, ok := h.users.get(sess.Username)
	return user, ok
}

func (h *webHandler) api(w http.ResponseWriter, r *http.Request, user account) {
	if r.URL.Path == "/api/me" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, publicAccount{user.Username, user.Role, user.MustChange})
		return
	}
	if r.URL.Path == "/api/password" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var request struct {
			Current string `json:"current"`
			New     string `json:"new"`
		}
		if err := decodeJSON(w, r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := h.users.changePassword(user.Username, request.Current, request.New, user.MustChange); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		h.sessionMu.Lock()
		for token, sess := range h.sessions {
			if sess.Username == user.Username {
				delete(h.sessions, token)
			}
		}
		h.sessionMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/api/users" {
		if user.Role != "admin" {
			writeError(w, 403, "需要管理员权限")
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, h.users.list())
		case http.MethodPost:
			var request struct {
				Username string `json:"username"`
				Role     string `json:"role"`
			}
			if err := decodeJSON(w, r, &request); err != nil {
				writeError(w, 400, err.Error())
				return
			}
			temp, err := randomPassword()
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			created, err := h.users.add(request.Username, request.Role, temp, true)
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
			writeJSON(w, 201, map[string]any{"user": publicAccount{created.Username, created.Role, created.MustChange}, "temporaryPassword": temp})
		default:
			methodNotAllowed(w)
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/users/") {
		if user.Role != "admin" {
			writeError(w, 403, "需要管理员权限")
			return
		}
		username := strings.TrimPrefix(r.URL.Path, "/api/users/")
		if !validUsername(username) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodDelete {
			methodNotAllowed(w)
			return
		}
		if username == user.Username {
			writeError(w, 400, "不能删除当前登录账号")
			return
		}
		if err := h.users.remove(username); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		h.sessionMu.Lock()
		for token, sess := range h.sessions {
			if sess.Username == username {
				delete(h.sessions, token)
			}
		}
		h.sessionMu.Unlock()
		w.WriteHeader(204)
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
	dataDir := os.Getenv("FRP_MANAGER_DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}
	users := newUserStore(dataDir)
	if err := users.load(); err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "admin-init":
			username := "admin"
			if len(os.Args) > 2 {
				username = os.Args[2]
			}
			if _, exists := users.get(strings.ToLower(username)); exists {
				log.Fatal("账号已存在")
			}
			password, err := randomPassword()
			if err != nil {
				log.Fatal(err)
			}
			if _, err := users.add(username, "admin", password, true); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("管理员账号: %s\n临时密码: %s\n请首次登录后立即修改密码。\n", strings.ToLower(username), password)
			return
		case "user-add":
			if len(os.Args) < 4 {
				log.Fatal("用法: frp-manager user-add <用户名> <user|admin>")
			}
			password, err := randomPassword()
			if err != nil {
				log.Fatal(err)
			}
			u, err := users.add(os.Args[2], os.Args[3], password, true)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Printf("用户名: %s\n临时密码: %s\n", u.Username, password)
			return
		case "user-reset":
			if len(os.Args) < 3 {
				log.Fatal("用法: frp-manager user-reset <用户名>")
			}
			password, err := randomPassword()
			if err != nil {
				log.Fatal(err)
			}
			if err := users.resetPassword(strings.ToLower(os.Args[2]), password); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("用户名: %s\n临时密码: %s\n", strings.ToLower(os.Args[2]), password)
			return
		case "user-delete":
			if len(os.Args) < 3 {
				log.Fatal("用法: frp-manager user-delete <用户名>")
			}
			if err := users.remove(strings.ToLower(os.Args[2])); err != nil {
				log.Fatal(err)
			}
			fmt.Println("账号已删除")
			return
		default:
			log.Fatalf("未知命令: %s", os.Args[1])
		}
	}
	if len(users.list()) == 0 {
		log.Fatal("尚无管理账号。请先运行: frp-manager admin-init [用户名]")
	}
	addr := os.Getenv("FRP_MANAGER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	frpc := os.Getenv("FRPC_BINARY")
	if frpc == "" {
		frpc = "frpc"
	}
	m := newManager(dataDir, frpc)
	if err := m.load(); err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Addr: addr, Handler: &webHandler{manager: m, users: users, sessions: map[string]session{}}, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("FRP manager listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
