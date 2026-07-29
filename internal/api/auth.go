package api

// HomeForge authentication — HA-style: HomeForge owns the login, so ALL access paths (the
// Cloudflare tunnel, the WireGuard VPN, and the local LAN) are protected by the same gate, not
// just the cloud edge. Humans get a session cookie via password login; device/ingest endpoints
// and server-internal calls stay open so the ~20 integrations don't break.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/whimtrav/homeforge/internal/config"
	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "hf_session"

type authUser struct {
	Email   string `json:"email"`
	Hash    string `json:"hash"`
	Created string `json:"created"`
}
type authSession struct {
	Email   string `json:"email"`
	Expires int64  `json:"expires"`
}

type authStore struct {
	mu          sync.Mutex
	enabled     bool
	ownerEmail  string
	usersPath   string
	sessPath    string
	sessionDays int
	users       map[string]authUser
	sessions    map[string]authSession
	fails       map[string]int   // client ip -> recent failed logins
	blockUntil  map[string]int64 // client ip -> unix time
}

func newAuthStore(c config.AuthConfig) *authStore {
	a := &authStore{
		enabled:     c.Enabled,
		ownerEmail:  strings.ToLower(strings.TrimSpace(c.OwnerEmail)),
		usersPath:   c.UsersFile,
		sessPath:    strings.TrimSuffix(c.UsersFile, ".json") + "-sessions.json",
		sessionDays: c.SessionDays,
		users:       map[string]authUser{},
		sessions:    map[string]authSession{},
		fails:       map[string]int{},
		blockUntil:  map[string]int64{},
	}
	if a.sessionDays <= 0 {
		a.sessionDays = 30
	}
	a.load()
	return a
}

func (a *authStore) load() {
	if data, err := os.ReadFile(a.usersPath); err == nil {
		var us []authUser
		_ = json.Unmarshal(data, &us)
		for _, u := range us {
			a.users[strings.ToLower(u.Email)] = u
		}
	}
	if data, err := os.ReadFile(a.sessPath); err == nil {
		_ = json.Unmarshal(data, &a.sessions)
	}
}

func (a *authStore) persistUsers() {
	us := make([]authUser, 0, len(a.users))
	for _, u := range a.users {
		us = append(us, u)
	}
	data, _ := json.MarshalIndent(us, "", "  ")
	tmp := a.usersPath + ".tmp"
	if os.WriteFile(tmp, data, 0600) == nil {
		os.Rename(tmp, a.usersPath)
	}
}
func (a *authStore) persistSessions() {
	data, _ := json.Marshal(a.sessions)
	tmp := a.sessPath + ".tmp"
	if os.WriteFile(tmp, data, 0600) == nil {
		os.Rename(tmp, a.sessPath)
	}
}

func (a *authStore) hasUsers() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.users) > 0
}

func (a *authStore) createUser(email, password string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return fmt.Errorf("a valid email is required")
	}
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.users[email] = authUser{Email: email, Hash: string(hash), Created: time.Now().Format(time.RFC3339)}
	a.persistUsers()
	a.mu.Unlock()
	return nil
}

func (a *authStore) exists(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.users[email]
	return ok
}

func (a *authStore) setPassword(email, password string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[email]
	if !ok {
		return fmt.Errorf("no such user")
	}
	u.Hash = string(hash)
	a.users[email] = u
	a.persistUsers()
	return nil
}

func (a *authStore) list() []map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]map[string]string, 0, len(a.users))
	for _, u := range a.users {
		out = append(out, map[string]string{"email": u.Email, "created": u.Created})
	}
	return out
}

func (a *authStore) deleteUser(email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.users[email]; !ok {
		return fmt.Errorf("no such user")
	}
	if len(a.users) <= 1 {
		return fmt.Errorf("can't remove the last account")
	}
	delete(a.users, email)
	a.persistUsers()
	return nil
}

func (a *authStore) verify(email, password string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	a.mu.Lock()
	u, ok := a.users[email]
	a.mu.Unlock()
	if !ok {
		// run a dummy compare so timing doesn't reveal whether the email exists
		bcrypt.CompareHashAndPassword([]byte("$2a$10$0000000000000000000000000000000000000000000000000000"), []byte(password))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(password)) == nil
}

func randToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *authStore) newSession(email string) string {
	tok := randToken()
	a.mu.Lock()
	a.sessions[tok] = authSession{Email: email, Expires: time.Now().AddDate(0, 0, a.sessionDays).Unix()}
	a.persistSessions()
	a.mu.Unlock()
	return tok
}

func (a *authStore) valid(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[tok]
	if !ok {
		return "", false
	}
	if time.Now().Unix() > s.Expires {
		delete(a.sessions, tok)
		return "", false
	}
	return s.Email, true
}

func (a *authStore) deleteSession(tok string) {
	a.mu.Lock()
	delete(a.sessions, tok)
	a.persistSessions()
	a.mu.Unlock()
}

// ── simple per-IP login rate limiting (the login is internet-facing via the tunnel) ──
func (a *authStore) blocked(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	until, ok := a.blockUntil[ip]
	return ok && time.Now().Unix() < until
}
func (a *authStore) recordFail(ip string) {
	a.mu.Lock()
	a.fails[ip]++
	if a.fails[ip] >= 8 {
		a.blockUntil[ip] = time.Now().Add(10 * time.Minute).Unix()
		a.fails[ip] = 0
	}
	a.mu.Unlock()
}
func (a *authStore) recordOK(ip string) {
	a.mu.Lock()
	delete(a.fails, ip)
	delete(a.blockUntil, ip)
	a.mu.Unlock()
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func secureCookie(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ── Server wiring ──

// SetAuth installs the auth store and a per-process internal token (lets server-internal HTTP
// calls bypass the gate). Call before Start so the middleware can wrap the mux.
func (s *Server) SetAuth(c config.AuthConfig) {
	s.auth = newAuthStore(c)
	s.internalToken = randToken()
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil || !s.auth.enabled || s.authAllowed(r) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// authAllowed: the SPA shell + static assets are public; sensitive data/control lives under
// /api and needs a session — except the auth endpoints, device ingest, and internal calls.
func (s *Server) authAllowed(r *http.Request) bool {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/") {
		return true
	}
	// only the public auth endpoints bypass the gate; account management (/api/auth/users,
	// change-password, …) still requires a session.
	switch p {
	case "/api/auth/me", "/api/auth/login", "/api/auth/setup", "/api/auth/logout":
		return true
	}
	if r.Method == http.MethodPost && (p == "/api/health" || p == "/api/comfort" ||
		strings.HasPrefix(p, "/api/scale/") || strings.HasPrefix(p, "/api/ble/")) {
		return true
	}
	if s.internalToken != "" && r.Header.Get("X-HF-Internal") == s.internalToken {
		return true
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, ok := s.auth.valid(c.Value); ok {
			return true
		}
	}
	return false
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, email string) {
	tok := s.auth.newSession(email)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secureCookie(r),
		Expires: time.Now().AddDate(0, 0, s.auth.sessionDays),
	})
}

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	authed, email := false, ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		if e, ok := s.auth.valid(c.Value); ok {
			authed, email = true, e
		}
	}
	writeJSON(w, map[string]any{
		"authenticated": authed, "email": email,
		"needsSetup": !s.auth.hasUsers(), "ownerEmail": s.auth.ownerEmail, "enabled": s.auth.enabled,
	})
}

func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	if s.auth.hasUsers() {
		http.Error(w, "already set up", http.StatusForbidden)
		return
	}
	var b struct{ Email, Password string }
	json.NewDecoder(r.Body).Decode(&b)
	if strings.TrimSpace(b.Email) == "" {
		b.Email = s.auth.ownerEmail
	}
	if err := s.auth.createUser(b.Email, b.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.setSession(w, r, strings.ToLower(strings.TrimSpace(b.Email)))
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.auth.blocked(ip) {
		http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
		return
	}
	var b struct{ Email, Password string }
	json.NewDecoder(r.Body).Decode(&b)
	if !s.auth.verify(b.Email, b.Password) {
		s.auth.recordFail(ip)
		http.Error(w, "invalid email or password", http.StatusUnauthorized)
		return
	}
	s.auth.recordOK(ip)
	s.setSession(w, r, strings.ToLower(strings.TrimSpace(b.Email)))
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.auth.deleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) sessionEmail(r *http.Request) (string, bool) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return s.auth.valid(c.Value)
	}
	return "", false
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	email, ok := s.sessionEmail(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var b struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	if !s.auth.verify(email, b.CurrentPassword) {
		http.Error(w, "current password is incorrect", http.StatusBadRequest)
		return
	}
	if err := s.auth.setPassword(email, b.NewPassword); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionEmail(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, s.auth.list())
}

func (s *Server) handleAddUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionEmail(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var b struct{ Email, Password string }
	json.NewDecoder(r.Body).Decode(&b)
	if s.auth.exists(b.Email) {
		http.Error(w, "an account with that email already exists", http.StatusBadRequest)
		return
	}
	if err := s.auth.createUser(b.Email, b.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	me, ok := s.sessionEmail(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	target := strings.ToLower(strings.TrimSpace(r.PathValue("email")))
	if target == me {
		http.Error(w, "you can't remove your own account", http.StatusBadRequest)
		return
	}
	if err := s.auth.deleteUser(target); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
