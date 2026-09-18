package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// sessionCookieName is the opaque, httpOnly, SameSite=Lax session cookie.
// It holds only the session ID; no token, OAuth state, or other secret is ever
// placed in a cookie.
const sessionCookieName = "gitsafe_session"

// sessionTTL bounds how long a session lives before it must be re-established.
// OAuth state and CSRF tokens are scoped to the session and expire with it.
const sessionTTL = 2 * time.Hour

// session is the server-side state bound to an opaque cookie. It is held only
// in memory — never serialized to state.json or anywhere else.
type session struct {
	id         string
	csrf       string
	ghState    string // GitHub OAuth state bound to this session (single-use)
	ghStateUs  bool   // whether the GitHub OAuth state has already been consumed
	drvState   string // Drive OAuth state bound to this session (single-use)
	drvStateUs bool   // whether the Drive OAuth state has already been consumed
	expiresAt  time.Time
}

// sessionManager tracks in-memory sessions. It is concurrency-safe and prunes
// expired sessions lazily. Sessions never survive a restart.
type sessionManager struct {
	mu       sync.Mutex
	sessions map[string]*session
	now      func() time.Time
}

func newSessionManager() *sessionManager {
	return &sessionManager{sessions: map[string]*session{}, now: time.Now}
}

// get returns the session for id if present and not expired.
func (m *sessionManager) get(id string) (*session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	if !s.expiresAt.After(m.now()) {
		delete(m.sessions, id)
		return nil, false
	}
	return s, true
}

// put stores/refreshes a session.
func (m *sessionManager) put(s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	s.expiresAt = m.now().Add(sessionTTL)
	m.sessions[s.id] = s
}

// pruneLocked removes expired sessions. Caller must hold the lock.
func (m *sessionManager) pruneLocked() {
	now := m.now()
	for id, s := range m.sessions {
		if !s.expiresAt.After(now) {
			delete(m.sessions, id)
		}
	}
}

// consume github OAuth state: returns true and invalidates it on a matching,
// unused value; returns false otherwise. Single-use semantics.
func (m *sessionManager) consumeGitHubState(id, state string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.ghStateUs || s.ghState == "" {
		return false
	}
	s.ghStateUs = true
	return subtle.ConstantTimeCompare([]byte(s.ghState), []byte(state)) == 1
}

// consumeDriveState is the Drive-analogue of consumeGitHubState: matching,
// unused, single-use state values validate exactly once.
func (m *sessionManager) consumeDriveState(id, state string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || s.drvStateUs || s.drvState == "" {
		return false
	}
	s.drvStateUs = true
	return subtle.ConstantTimeCompare([]byte(s.drvState), []byte(state)) == 1
}

// randToken returns a cryptographically random hex string.
func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("session: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// ensureSession reads the session cookie, creating a new session if none is
// present, and sets the cookie on the response when it was freshly created.
func (s *Server) ensureSession(w http.ResponseWriter, r *http.Request) *session {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if sess, ok := s.sessions.get(cookie.Value); ok {
			return sess
		}
	}
	sess := &session{
		id:   randToken(32),
		csrf: randToken(32),
	}
	s.sessions.put(sess)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sess.id,
		Path:     "/",
		HttpOnly: true,
		Secure:   false, // local-only 127.0.0.1 server
		// Lax keeps the cookie off cross-site subresource/POST requests (CSRF)
		// while still attaching it to the top-level GET navigation that the
		// OAuth callback uses when returning from the provider to 127.0.0.1.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return sess
}

// sessionFromRequest returns the session for the request cookie, or nil.
func (s *Server) sessionFromRequest(r *http.Request) *session {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil
	}
	sess, ok := s.sessions.get(cookie.Value)
	if !ok {
		return nil
	}
	return sess
}

// validateCSRF checks the X-CSRF-Token header against the session's CSRF token.
func validateCSRF(sess *session, token string) bool {
	if sess == nil || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sess.csrf), []byte(token)) == 1
}
