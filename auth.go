package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName = "f95_session"
	sessionTTL = 14 * 24 * time.Hour
)

var userRe = regexp.MustCompile(`^[A-Za-z0-9._-]{3,32}$`)

func validCreds(user, pass string) error {
	if !userRe.MatchString(user) {
		return errors.New("username must be 3-32 characters: letters, digits, . _ -")
	}
	return validPassword(pass)
}

func validPassword(pass string) error {
	if len(pass) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	if len(pass) > 72 {
		return errors.New("password must be at most 72 bytes")
	}
	return nil
}

func hashPassword(p string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(p), 12)
	return string(b), err
}

func tokenHash(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

// ── login throttling (per client IP) ──

type throttle struct {
	mu sync.Mutex
	m  map[string]*attempt
}
type attempt struct {
	fails int
	until time.Time
	last  time.Time
}

func (t *throttle) blocked(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.m[ip]
	return a != nil && time.Now().Before(a.until)
}

func (t *throttle) fail(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*attempt{}
	}
	a := t.m[ip]
	if a == nil || time.Since(a.last) > 15*time.Minute {
		a = &attempt{}
		t.m[ip] = a
	}
	a.fails++
	a.last = time.Now()
	if a.fails >= 5 {
		a.until = time.Now().Add(5 * time.Minute)
		a.fails = 0
	}
}

func (t *throttle) ok(ip string) {
	t.mu.Lock()
	delete(t.m, ip)
	t.mu.Unlock()
}

func clientIP(r *http.Request) string {
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		return strings.TrimSpace(strings.Split(x, ",")[0])
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ── session helpers ──

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, uid int64) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	if err := s.db.CreateSession(uid, tokenHash(tok), sessionTTL, clientIP(r), trunc(r.UserAgent(), 200)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: isHTTPS(r), MaxAge: int(sessionTTL.Seconds()),
	})
	return nil
}

func (s *Server) currentUser(r *http.Request) (uid int64, name, tokHash string, ok bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return
	}
	tokHash = tokenHash(c.Value)
	uid, name, ok = s.db.SessionUser(tokHash)
	return
}

// auth protects a handler: unauthenticated pages go to /login, API calls get 401.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, _, ok := s.currentUser(r); !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, 401, map[string]any{"ok": false, "error": "not signed in", "setup_required": s.db.UserCount() == 0})
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-F95-UI") != "1" {
			http.Error(w, "missing X-F95-UI header", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// ── handlers ──

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, _, ok := s.currentUser(r); ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	b, _ := webFS.ReadFile("web/login.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

func (s *Server) indexPage(w http.ResponseWriter, r *http.Request) {
	b, _ := webFS.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	_, name, _, ok := s.currentUser(r)
	writeJSON(w, 200, map[string]any{"authenticated": ok, "username": name, "setup_required": s.db.UserCount() == 0})
}

type credsBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func csrfOK(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-F95-UI") != "1" {
		http.Error(w, "missing X-F95-UI header", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(w, r) {
		return
	}
	var b credsBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&b); err != nil {
		writeErr(w, 400, err)
		return
	}
	b.Username = strings.TrimSpace(b.Username)
	if err := validCreds(b.Username, b.Password); err != nil {
		writeErr(w, 400, err)
		return
	}
	h, err := hashPassword(b.Password)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	id, created, err := s.db.CreateFirstUser(b.Username, h)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if !created {
		writeErr(w, 409, errors.New("an administrator account already exists"))
		return
	}
	s.app.log.Info("Administrator account '%s' created via web setup", b.Username)
	if err := s.startSession(w, r, id); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// a valid bcrypt hash used to keep timing similar for unknown users
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), 12)

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(w, r) {
		return
	}
	ip := clientIP(r)
	if s.thr.blocked(ip) {
		writeErr(w, 429, errors.New("too many failed attempts - try again in a few minutes"))
		return
	}
	var b credsBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&b); err != nil {
		writeErr(w, 400, err)
		return
	}
	id, hash, found := s.db.GetUser(strings.TrimSpace(b.Username))
	if !found {
		hash = string(dummyHash)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(b.Password)) != nil || !found {
		s.thr.fail(ip)
		s.app.log.Warn("Failed login attempt for '%s' from %s", trunc(b.Username, 32), ip)
		time.Sleep(400 * time.Millisecond)
		writeErr(w, 401, errors.New("invalid username or password"))
		return
	}
	s.thr.ok(ip)
	if err := s.startSession(w, r, id); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.app.log.Info("User '%s' signed in from %s", b.Username, ip)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, _, th, ok := s.currentUser(r); ok {
		s.db.DeleteSession(th)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	uid, name, th, _ := s.currentUser(r)
	var b struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&b); err != nil {
		writeErr(w, 400, err)
		return
	}
	hash, _ := s.db.UserHash(uid)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(b.Current)) != nil {
		writeErr(w, 403, errors.New("current password is incorrect"))
		return
	}
	if err := validPassword(b.New); err != nil {
		writeErr(w, 400, err)
		return
	}
	nh, err := hashPassword(b.New)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := s.db.SetPassword(uid, nh); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.db.DeleteOtherSessions(uid, th) // sign out everywhere else
	s.app.log.Info("Password changed for '%s' (other sessions signed out)", name)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// bootstrapAdmin creates the account from ADMIN_USER/ADMIN_PASSWORD when none exists (unattended deploys).
func bootstrapAdmin(db *DB, user, pass string, log *LogHub) {
	if user == "" || pass == "" || db.UserCount() > 0 {
		return
	}
	if err := validCreds(user, pass); err != nil {
		log.Error("ADMIN_USER/ADMIN_PASSWORD ignored: %v", err)
		return
	}
	h, err := hashPassword(pass)
	if err != nil {
		return
	}
	if _, ok, _ := db.CreateFirstUser(user, h); ok {
		log.Info("Administrator account '%s' created from ADMIN_USER/ADMIN_PASSWORD", user)
	}
}
