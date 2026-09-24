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
	mu       sync.Mutex
	requests map[string][]time.Time
	maxReqs  int
	window   time.Duration
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
			w.Header().Set("X-RateLimit-Remaining", string(rune('0'+remaining)))
			if !allowed {
				w.Header().Set("X-RateLimit-Limit", string(rune('0'+maxReqs)))
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
// For unauthenticated requests, uses the client IP (from X-Forwarded-For if available).
func (s *Server) getRateLimitKey(r *http.Request) string {
	// Try to get user ID from session
	sess := s.sessionFromRequest(r)
	if sess != nil && sess.userID != 0 {
		return "user:" + strconv.FormatInt(sess.userID, 10)
	}
	// Fall back to IP-based rate limiting
	ip := clientIP(r)
	return "ip:" + ip
}

// clientIP extracts the client IP from the request.
// Uses X-Forwarded-For if available, otherwise RemoteAddr.
func clientIP(r *http.Request) string {
	// Check X-Forwarded-For header (may contain multiple IPs)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, ip := range strings.Split(xff, ",") {
			trimmed := strings.TrimSpace(ip)
			if trimmed != "" {
				return trimmed
			}
		}
	}
	// Fall back to RemoteAddr
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
