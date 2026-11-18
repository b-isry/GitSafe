package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Sessions and OAuth state are carried in signed (HMAC-SHA256) cookies rather
// than in process memory.
//
// Why: the previous design kept sessions and the single-use OAuth state in an
// in-memory map. On Render's free tier the process restarts on every deploy
// and spins down after ~15 minutes without traffic, and any restart between
// "Connect with GitHub" and GitHub redirecting back wiped the map, so the
// callback found no session and failed with "session expired; start over".
// Signed cookies survive restarts (the key is derived from GITSAFE_TOKEN_KEY,
// or GITSAFE_SESSION_KEY when set), need no database round trip, and keep the
// OAuth state bound to the browser that started the flow.
//
// Cookies are integrity-protected, not encrypted: they contain only a random
// session id, the CSRF token, the GitHub numeric user id, and an expiry. No
// OAuth token or other secret is ever placed in a cookie.

// sessionCookieName is the httpOnly, SameSite=Lax session cookie.
const sessionCookieName = "gitsafe_session"

// sessionTTL bounds how long a signed-in session lives before the user must
// sign in again. It is absolute (not sliding).
const sessionTTL = 7 * 24 * time.Hour

// oauthStateTTL bounds how long a started OAuth flow can take (time spent on
// GitHub's / Google's consent screen, including signing in there).
const oauthStateTTL = 15 * time.Minute

// oauthStateCookiePrefix prefixes the per-flow OAuth state cookies. Each flow
// gets its own cookie (named after its state value), so starting a second
// sign-in in another tab no longer invalidates the first.
const oauthStateCookiePrefix = "gitsafe_oauth_"

// oauthStateCookiePath scopes state cookies to the OAuth endpoints only.
const oauthStateCookiePath = "/api/auth/"

// Cookie purposes; they are mixed into the MAC so one kind of cookie can never
// be replayed as another.
const (
	purposeSession = "session/v1"
	purposeState   = "oauth-state/v1"
)

// session is the per-browser state carried by the signed session cookie.
type session struct {
	id        string
	csrf      string
	userID    int64 // GitHub user ID of the authenticated user (0 if not authenticated)
	expiresAt time.Time
}

type sessionPayload struct {
	ID     string `json:"i"`
	CSRF   string `json:"c"`
	UserID int64  `json:"u,omitempty"`
	Exp    int64  `json:"e"`
}

// oauthState is the payload of an OAuth state cookie.
type oauthState struct {
	Provider string `json:"p"`
	Nonce    string `json:"n"`
	UserID   int64  `json:"u,omitempty"`
	Exp      int64  `json:"e"`
}

// sessionManager signs and verifies session and OAuth-state cookies. It holds
// no per-session data, only the signing key and a short-lived record of OAuth
// states already consumed (replay protection within this process; GitHub and
// Google authorization codes are single-use anyway).
type sessionManager struct {
	key    []byte
	secure bool
	now    func() time.Time

	mu   sync.Mutex
	used map[string]time.Time // consumed OAuth nonces -> expiry
}

// newSessionManager derives the cookie signing key from keyMaterial. An empty
// keyMaterial (local development without GITSAFE_TOKEN_KEY) uses a random
// per-process key, so sessions then end on restart. secure sets the Secure
// cookie attribute (true whenever the base URL is https).
func newSessionManager(keyMaterial string, secure bool) *sessionManager {
	var key []byte
	if keyMaterial == "" {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic("session: crypto/rand unavailable: " + err.Error())
		}
	} else {
		mac := hmac.New(sha256.New, []byte(keyMaterial))
		mac.Write([]byte("gitsafe/cookie-signing/v1"))
		key = mac.Sum(nil)
	}
	return &sessionManager{key: key, secure: secure, now: time.Now, used: map[string]time.Time{}}
}

func (m *sessionManager) sign(purpose string, payload any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic("session: marshal payload: " + err.Error())
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + base64.RawURLEncoding.EncodeToString(m.mac(purpose, body))
}

func (m *sessionManager) mac(purpose, body string) []byte {
	h := hmac.New(sha256.New, m.key)
	h.Write([]byte(purpose))
	h.Write([]byte{0})
	h.Write([]byte(body))
	return h.Sum(nil)
}

var (
	errCookieMalformed = errors.New("malformed")
	errCookieSignature = errors.New("bad_signature")
)

func (m *sessionManager) verify(purpose, value string, out any) error {
	body, sig, ok := strings.Cut(value, ".")
	if !ok || body == "" || sig == "" {
		return errCookieMalformed
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return errCookieMalformed
	}
	if !hmac.Equal(gotSig, m.mac(purpose, body)) {
		return errCookieSignature
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return errCookieMalformed
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errCookieMalformed
	}
	return nil
}

// issue returns the signed cookie value for sess, setting its expiry when
// unset.
func (m *sessionManager) issue(sess *session) string {
	if sess.expiresAt.IsZero() {
		sess.expiresAt = m.now().Add(sessionTTL)
	}
	return m.sign(purposeSession, sessionPayload{
		ID: sess.id, CSRF: sess.csrf, UserID: sess.userID, Exp: sess.expiresAt.Unix(),
	})
}

// get verifies a session cookie value and returns its session.
func (m *sessionManager) get(value string) (*session, bool) {
	var p sessionPayload
	if err := m.verify(purposeSession, value, &p); err != nil {
		return nil, false
	}
	exp := time.Unix(p.Exp, 0)
	if !exp.After(m.now()) || p.ID == "" || p.CSRF == "" || p.UserID < 0 {
		return nil, false
	}
	return &session{id: p.ID, csrf: p.CSRF, userID: p.UserID, expiresAt: exp}, true
}

// newOAuthState mints a state value and its signed cookie payload.
func (m *sessionManager) newOAuthState(provider string, userID int64) (nonce, cookieValue string) {
	nonce = randToken(32)
	return nonce, m.sign(purposeState, oauthState{
		Provider: provider, Nonce: nonce, UserID: userID, Exp: m.now().Add(oauthStateTTL).Unix(),
	})
}

// consumeOAuthState validates the state returned by the provider against the
// signed cookie from the browser and marks it used. reason is a short,
// non-secret label for logs.
func (m *sessionManager) consumeOAuthState(provider, stateParam, cookieValue string) (st oauthState, reason string, ok bool) {
	if cookieValue == "" {
		return oauthState{}, "no_state_cookie", false
	}
	if err := m.verify(purposeState, cookieValue, &st); err != nil {
		return oauthState{}, "state_cookie_" + err.Error(), false
	}
	if st.Provider != provider {
		return oauthState{}, "state_provider_mismatch", false
	}
	now := m.now()
	if !time.Unix(st.Exp, 0).After(now) {
		return oauthState{}, "state_expired", false
	}
	if subtle.ConstantTimeCompare([]byte(st.Nonce), []byte(stateParam)) != 1 {
		return oauthState{}, "state_mismatch", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for n, exp := range m.used {
		if !exp.After(now) {
			delete(m.used, n)
		}
	}
	if _, seen := m.used[st.Nonce]; seen {
		return oauthState{}, "state_reused", false
	}
	m.used[st.Nonce] = time.Unix(st.Exp, 0)
	return st, "", true
}

// oauthStateCookieName derives the per-flow cookie name from the state value
// (hex, so it is a valid cookie name). Returns "" for a malformed state.
func oauthStateCookieName(provider, state string) string {
	if len(state) < 16 {
		return ""
	}
	prefix := state[:16]
	if _, err := hex.DecodeString(prefix); err != nil {
		return ""
	}
	return oauthStateCookiePrefix + provider + "_" + prefix
}

// randToken returns a cryptographically random hex string.
func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("session: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// setSessionCookie writes the signed session cookie.
func (s *Server) setSessionCookie(w http.ResponseWriter, sess *session) {
	value := s.sessions.issue(sess)
	maxAge := int(time.Until(sess.expiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.sessions.secure,
		// Lax keeps the cookie off cross-site subresource/POST requests (CSRF)
		// while still sending it on the top-level GET navigation the OAuth
		// provider uses to return to the callback.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	})
}

// clearSessionCookie deletes the session cookie (logout, account deletion).
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: s.sessions.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// newSession creates a fresh session (new id and CSRF token) for userID.
func newSession(userID int64) *session {
	return &session{id: randToken(16), csrf: randToken(32), userID: userID}
}

// ensureSession returns the request's session, creating an anonymous one (and
// setting its cookie) when there is no valid session cookie.
func (s *Server) ensureSession(w http.ResponseWriter, r *http.Request) *session {
	if sess := s.sessionFromRequest(r); sess != nil {
		return sess
	}
	sess := newSession(0)
	s.setSessionCookie(w, sess)
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

// startOAuthFlow sets the per-flow state cookie and returns the state value
// to send to the provider.
func (s *Server) startOAuthFlow(w http.ResponseWriter, provider string, userID int64) string {
	nonce, value := s.sessions.newOAuthState(provider, userID)
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName(provider, nonce),
		Value:    value,
		Path:     oauthStateCookiePath,
		HttpOnly: true,
		Secure:   s.sessions.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthStateTTL.Seconds()),
	})
	return nonce
}

// finishOAuthFlow validates the callback's state against the browser's state
// cookie and deletes that cookie.
func (s *Server) finishOAuthFlow(w http.ResponseWriter, r *http.Request, provider string) (oauthState, string, bool) {
	stateParam := r.URL.Query().Get("state")
	name := oauthStateCookieName(provider, stateParam)
	if name == "" {
		return oauthState{}, "malformed_state_param", false
	}
	var value string
	if c, err := r.Cookie(name); err == nil {
		value = c.Value
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: oauthStateCookiePath, HttpOnly: true,
			Secure: s.sessions.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
		})
	}
	return s.sessions.consumeOAuthState(provider, stateParam, value)
}

// validateCSRF checks the X-CSRF-Token header against the session's CSRF token.
func validateCSRF(sess *session, token string) bool {
	if sess == nil || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sess.csrf), []byte(token)) == 1
}
