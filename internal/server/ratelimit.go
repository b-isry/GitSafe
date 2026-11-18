package server

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rateLimiter is a simple in-memory token bucket rate limiter.
// It tracks request counts per key (user ID or IP) in a sliding window.
type rateLimiter struct {
	mu        sync.Mutex
	requests  map[string][]time.Time
	maxReqs   int
	window    time.Duration
	lastPrune time.Time
}

func newRateLimiter(maxReqs int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		requests: make(map[string][]time.Time),
		maxReqs:  maxReqs,
		window:   window,
	}
}

// allow checks if the request should be allowed. It returns true if allowed,
// false if rate limited. It also returns the remaining requests in the window.
func (rl *rateLimiter) allow(key string) (bool, int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Drop keys whose requests have all aged out, at most once per window, so
	// the map does not grow forever with every client IP ever seen.
	if now.Sub(rl.lastPrune) >= rl.window {
		for k, ts := range rl.requests {
			if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
				delete(rl.requests, k)
			}
		}
		rl.lastPrune = now
	}

	// Filter out old requests
	times := rl.requests[key]
	var valid []time.Time
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	if len(valid) >= rl.maxReqs {
		rl.requests[key] = valid
		return false, 0
	}

	valid = append(valid, now)
	rl.requests[key] = valid
	return true, rl.maxReqs - len(valid)
}

// rateLimitMiddleware returns a middleware that applies rate limiting.
// keyFunc extracts the rate limit key from the request (user ID or IP).
func (s *Server) rateLimitMiddleware(maxReqs int, window time.Duration, keyFunc func(*http.Request) string) func(http.Handler) http.Handler {
	limiter := newRateLimiter(maxReqs, window)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyFunc(r)
			allowed, remaining := limiter.allow(key)
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			if !allowed {
				w.Header().Set("X-RateLimit-Limit", strconv.Itoa(maxReqs))
				w.Header().Set("Retry-After", "60")
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// getRateLimitKey extracts the rate limit key from the request.
// For authenticated requests, uses the user's GitHub ID from the session.
// For unauthenticated requests, uses the client IP.
func (s *Server) getRateLimitKey(r *http.Request) string {
	// Try to get user ID from session
	sess := s.sessionFromRequest(r)
	if sess != nil && sess.userID != 0 {
		return "user:" + strconv.FormatInt(sess.userID, 10)
	}
	var trustedHeader string
	if s != nil && s.app != nil {
		trustedHeader = s.app.Config.TrustedProxyHeader
	}
	ip := clientIP(r, trustedHeader)
	return "ip:" + ip
}

// remoteAddrHost extracts the host part of remoteAddr using net.SplitHostPort,
// falling back to the raw value if splitting fails.
func remoteAddrHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// clientIP extracts the client IP from the request.
// By default it uses the host part of RemoteAddr only.
// When trustedHeader is configured with a header name (for example CF-Connecting-IP or X-Real-IP),
// it reads that header, validates it with net.ParseIP, and falls back to RemoteAddr
// if missing or invalid. X-Forwarded-For is never read unless explicitly named in trustedHeader,
// in which case the rightmost entry is used.
func clientIP(r *http.Request, trustedHeader string) string {
	fallback := remoteAddrHost(r.RemoteAddr)
	trustedHeader = strings.TrimSpace(trustedHeader)
	if trustedHeader == "" {
		return fallback
	}

	val := r.Header.Get(trustedHeader)
	if val == "" {
		return fallback
	}

	var candidate string
	if strings.EqualFold(trustedHeader, "X-Forwarded-For") {
		parts := strings.Split(val, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			p := strings.TrimSpace(parts[i])
			if p != "" {
				candidate = p
				break
			}
		}
	} else {
		candidate = strings.TrimSpace(val)
	}

	if candidate == "" {
		return fallback
	}

	candidateHost := candidate
	if h, _, err := net.SplitHostPort(candidate); err == nil {
		candidateHost = h
	}

	if net.ParseIP(candidateHost) == nil {
		return fallback
	}

	return candidateHost
}
