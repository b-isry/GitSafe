package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// authedKeyRequest is a request carrying a session cookie bound to the given
// user, mimicking what a real signed-in browser sends.
func authedKeyRequest(t *testing.T, s *Server, userID int64) *http.Request {
	t.Helper()
	cookie, _ := authedCookie(t, s, userID)
	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	return req
}

// TestGetRateLimitKeyAuthenticatedUser verifies the key is a stable decimal
// "user:<GitHub ID>" derived from the session, including IDs beyond the
// Unicode code-point range that the previous rune formatting corrupted.
func TestGetRateLimitKeyAuthenticatedUser(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)

	for _, tc := range []struct {
		userID int64
		want   string
	}{
		{42, "user:42"},
		{100000000, "user:100000000"},
	} {
		req := authedKeyRequest(t, s, tc.userID)
		if got := s.getRateLimitKey(req); got != tc.want {
			t.Errorf("userID %d: key = %q, want %q", tc.userID, got, tc.want)
		}
	}
}

// TestGetRateLimitKeyDistinctForLargeUserIDs is the regression check for the
// old string(rune(userID)) formatting, which collapsed every userID above
// 0x10FFFF (1,114,111) into the single shared bucket "user:\uFFFD". Distinct
// large IDs must yield distinct keys.
func TestGetRateLimitKeyDistinctForLargeUserIDs(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)

	a := authedKeyRequest(t, s, 1114112)
	b := authedKeyRequest(t, s, 1200000)

	keyA := s.getRateLimitKey(a)
	keyB := s.getRateLimitKey(b)

	if sess, ok := s.sessions.get(sessionCookieValue(t, a)); !ok || sess.userID != 1114112 {
		t.Fatalf("expected an authenticated session for userID 1114112")
	}
	if sess, ok := s.sessions.get(sessionCookieValue(t, b)); !ok || sess.userID != 1200000 {
		t.Fatalf("expected an authenticated session for userID 1200000")
	}

	if keyA == keyB {
		t.Fatalf("large user IDs must not share a rate-limit key: both %q", keyA)
	}
	if keyA != "user:1114112" || keyB != "user:1200000" {
		t.Fatalf("keys = %q, %q, want %q, %q", keyA, keyB, "user:1114112", "user:1200000")
	}
}

// TestGetRateLimitKeyNoSessionFallsBackToIP verifies a request without any
// session is keyed by the client IP.
func TestGetRateLimitKeyNoSessionFallsBackToIP(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	if got := s.getRateLimitKey(req); got != "ip:127.0.0.1" {
		t.Fatalf("key without session = %q, want %q", got, "ip:127.0.0.1")
	}
}

// TestGetRateLimitKeyZeroUserIDFallsBackToIP verifies a session that has not
// completed a GitSafe login (userID stays 0) is treated as unauthenticated and
// keyed by IP, never by an empty or broken user tag.
func TestGetRateLimitKeyZeroUserIDFallsBackToIP(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	cookie := anonSessionCookie(t, s)
	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	if got := s.getRateLimitKey(req); got != "ip:127.0.0.1" {
		t.Fatalf("key with userID 0 session = %q, want %q", got, "ip:127.0.0.1")
	}

	// Confirm the 100000000 case actually had an authenticated session, so the
	// IP fallback test above is exercising userID 0, not a missing session bug.
	big := authedKeyRequest(t, s, 100000000)
	if sess, ok := s.sessions.get(sessionCookieValue(t, big)); !ok || sess.userID != 100000000 {
		t.Fatalf("expected an authenticated session for userID 100000000")
	}
}

func sessionCookieValue(t *testing.T, r *http.Request) string {
	t.Helper()
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		t.Fatal(err)
	}
	return c.Value
}
