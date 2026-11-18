package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
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

func TestRateLimitHeadersAreDecimal(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	h := s.rateLimitMiddleware(60, time.Minute, func(*http.Request) string { return "k" })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "59" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 59", got)
	}
}

func TestRateLimiterPrunesIdleKeys(t *testing.T) {
	rl := newRateLimiter(5, 10*time.Millisecond)
	for i := 0; i < 100; i++ {
		rl.allow(strconv.Itoa(i))
	}
	time.Sleep(20 * time.Millisecond)
	rl.allow("fresh")
	rl.mu.Lock()
	n := len(rl.requests)
	rl.mu.Unlock()
	if n != 1 {
		t.Fatalf("idle keys not pruned: %d keys remain", n)
	}
}

func TestClientIPSpoofedXForwardedForIgnoredByDefault(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.RemoteAddr = "198.51.100.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.195, 70.41.3.18")

	if got := s.getRateLimitKey(req); got != "ip:198.51.100.1" {
		t.Fatalf("spoofed X-Forwarded-For must be ignored by default, got %q, want %q", got, "ip:198.51.100.1")
	}
}

func TestClientIPTrustedHeaderHonored(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	s.app.Config.TrustedProxyHeader = "CF-Connecting-IP"

	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.RemoteAddr = "198.51.100.1:12345"
	req.Header.Set("CF-Connecting-IP", "203.0.113.195")

	if got := s.getRateLimitKey(req); got != "ip:203.0.113.195" {
		t.Fatalf("trusted header CF-Connecting-IP not honored, got %q, want %q", got, "ip:203.0.113.195")
	}

	// When X-Forwarded-For is explicitly trusted, the rightmost entry is used.
	reqXFF := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	reqXFF.RemoteAddr = "198.51.100.1:12345"
	reqXFF.Header.Set("X-Forwarded-For", "192.0.2.1, 203.0.113.50")
	if got := clientIP(reqXFF, "X-Forwarded-For"); got != "203.0.113.50" {
		t.Fatalf("rightmost X-Forwarded-For not used, got %q, want %q", got, "203.0.113.50")
	}
}

func TestClientIPInvalidHeaderFallsBackToRemoteAddr(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	s.app.Config.TrustedProxyHeader = "X-Real-IP"

	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.RemoteAddr = "198.51.100.1:12345"
	req.Header.Set("X-Real-IP", "not-a-valid-ip")

	if got := s.getRateLimitKey(req); got != "ip:198.51.100.1" {
		t.Fatalf("invalid header IP must fall back to RemoteAddr, got %q, want %q", got, "ip:198.51.100.1")
	}

	// Missing header also falls back to RemoteAddr.
	reqMissing := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	reqMissing.RemoteAddr = "198.51.100.1:12345"
	if got := s.getRateLimitKey(reqMissing); got != "ip:198.51.100.1" {
		t.Fatalf("missing header must fall back to RemoteAddr, got %q, want %q", got, "ip:198.51.100.1")
	}
}

func TestClientIPRemoteAddrIPv6(t *testing.T) {
	reqWithPort := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	reqWithPort.RemoteAddr = "[2001:db8::1]:54321"
	if got := clientIP(reqWithPort, ""); got != "2001:db8::1" {
		t.Fatalf("IPv6 with port failed, got %q, want %q", got, "2001:db8::1")
	}

	reqRaw := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	reqRaw.RemoteAddr = "2001:db8::1"
	if got := clientIP(reqRaw, ""); got != "2001:db8::1" {
		t.Fatalf("raw IPv6 failed, got %q, want %q", got, "2001:db8::1")
	}
}
