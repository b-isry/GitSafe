// Package notify delivers GitSafe operational notifications (cleanup run
// results) to external targets. The only implemented target is a configurable
// webhook; notifications are disabled by default and a delivery failure never
// fails the operation that produced it.
//
// Security contract:
//   - the configured auth token is never logged, never returned by any API, and
//     is sent only inside the configured header;
//   - response bodies are discarded as they arrive and are never logged or
//     surfaced;
//   - webhook destinations are SSRF-guarded: only http/https are allowed (no
//     userinfo/credentials embedded in the URL), and destinations that resolve
//     only to unspecified, link-local (incl. the cloud-metadata endpoint
//     169.254.169.254), or multicast addresses are rejected before any network
//     I/O. Loopback and private-network targets remain allowed — local webhooks
//     are a supported use case and the GitSafe API is loopback-bound. Redirects
//     are capped and re-run the same destination check on every hop;
//   - webhook URLs that fail to deliver are sanitized (scheme://host only) in
//     error strings so query strings or paths carrying secrets never reach
//     logs, history, or API responses.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/b-isry/gitsafe/internal/config"
)

// ErrDisabled is returned by Deliver when notifications are not configured for
// this delivery. Callers may ignore it; it is not a delivery failure.
var ErrDisabled = errors.New("notifications are disabled")

// Trigger values embedded in webhook payloads.
const (
	TriggerManual    = "manual"
	TriggerScheduled = "scheduled"
	TriggerTest      = "test"
)

// DefaultTimeout bounds a single webhook delivery when the config does not set
// WebhookTimeoutSeconds.
const DefaultTimeout = 10 * time.Second

// maxErrors caps how many error strings a payload carries so a pathological run
// cannot produce an unbounded payload.
const maxErrors = 10

// maxErrorLen caps each embedded error string.
const maxErrorLen = 500

// maxWebhookRedirects bounds redirected webhook hops. Every hop re-runs the SSRF
// destination guard (including a DNS lookup for hostnames), so a tight bound
// also bounds lookup work and total latency.
const maxWebhookRedirects = 5

// webhookDestinationBlocked is returned when a webhook URL can only reach
// destinations that are never legitimate notification targets. Ping the error
// text; callers snapshot it into errors and history.
var webhookDestinationBlocked = errors.New("webhook destination is not allowed (link-local, multicast, and unspecified addresses are blocked)")

// Event is a snapshot of a completed cleanup run ready for delivery.
type Event struct {
	Trigger       string
	StartedAt     time.Time
	FinishedAt    time.Time
	Success       bool
	Inspected     int
	Retained      int
	LocalDeleted  int
	DriveDeleted  int
	OrphanDeleted int
	Skipped       int
	Missing       int
	Errors        []string
}

// Settings is the resolved delivery target for one notification.
type Settings struct {
	URL        string
	AuthHeader string
	AuthToken  string
	Timeout    time.Duration
}

// ConfigFunc returns the current notification configuration. It is called on
// every delivery so a settings save takes effect immediately. Implementations
// must be safe for concurrent calls.
type ConfigFunc func() config.NotificationConfig

// Notifier delivers Events to the configured webhook.
type Notifier struct {
	cfg        ConfigFunc
	logger     *slog.Logger
	httpClient *http.Client
	clock      func() time.Time
	// resolveHost resolves a webhook host to its IP addresses for the SSRF
	// guard. It is a field so tests can simulate resolved addresses without
	// relying on the OS resolver. Nil falls back to the OS resolver.
	resolveHost func(ctx context.Context, host string) ([]netip.Addr, error)
}

// New returns a Notifier delivering to the configuration returned by cfg. The
// shared HTTP client uses the default transport; every request gets the
// configured per-delivery context timeout.
func New(cfg ConfigFunc, logger *slog.Logger) *Notifier {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	n := &Notifier{
		cfg:    cfg,
		logger: logger,
		clock:  time.Now,
	}
	n.resolveHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	// The base client has no global timeout; each delivery derives a bounded
	// context from the resolved per-delivery timeout instead, so a slow webhook
	// can never hang a delivery while settings stay responsive.
	n.httpClient = &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConnsPerHost:   http.DefaultMaxIdleConnsPerHost,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		},
		// Every redirect hop is bounded and re-checked with the same SSRF guard
		// so a compromised endpoint cannot hop GitSafe's outbound request to a
		// forbidden destination (or into an endless redirect loop).
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxWebhookRedirects {
				return fmt.Errorf("webhook redirected more than %d times", maxWebhookRedirects)
			}
			return checkRedirectDestination(req.Context(), req.URL, n.resolveHost)
		},
	}
	return n
}

// resolve turns the live configuration into concrete delivery settings, or
// returns ErrDisabled when notifications are off. Invalid configurations are
// rejected defensively (the config layer validates on save, but a hand-edited
// file could bypass that).
func (n *Notifier) resolve() (Settings, error) {
	direct := n.cfg()
	if !direct.Enabled {
		return Settings{}, ErrDisabled
	}
	if err := direct.Validate(); err != nil {
		return Settings{}, err
	}
	timeout := DefaultTimeout
	if direct.WebhookTimeoutSeconds > 0 {
		timeout = time.Duration(direct.WebhookTimeoutSeconds) * time.Second
	}
	// The auth token is expanded here (not at config load/save) so an operator
	// can reference an environment variable without ever persisting the resolved
	// secret. An unset variable expands to "" exactly as Load-time expansion did.
	authToken := strings.TrimSpace(os.ExpandEnv(direct.WebhookAuthToken))
	return Settings{
		URL:        direct.WebhookURL,
		AuthHeader: direct.WebhookAuthHeader,
		AuthToken:  authToken,
		Timeout:    timeout,
	}, nil
}

// Deliver sends ev to the configured target. It returns ErrDisabled when
// notifications are disabled; any other error describes the failed delivery.
// The HTTP response body is drained and discarded — it is an error channel, not
// content worth logging, and never appears in the returned error or the logs.
func (n *Notifier) Deliver(ctx context.Context, ev Event) error {
	settings, err := n.resolve()
	if err != nil {
		return err
	}

	body, err := json.Marshal(n.payload(ev))
	if err != nil {
		return fmt.Errorf("build notification payload: %w", err)
	}

	sendCtx := ctx
	if sendCtx == nil {
		sendCtx = context.Background()
	}
	sendCtx, cancel := context.WithTimeout(sendCtx, settings.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, settings.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %s", sanitizeWebhookError(err, settings.URL))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GitSafe/"+Version)
	if settings.AuthHeader != "" {
		req.Header.Set(settings.AuthHeader, settings.AuthToken)
	}

	// SSRF guard: never dial a destination that only resolves to forbidden
	// addresses. Runs before any network I/O.
	if err := checkDestination(sendCtx, req.URL, n.resolveHost); err != nil {
		return err
	}

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("webhook request failed: %s", sanitizeWebhookError(err, settings.URL))
	}
	defer resp.Body.Close()
	// Drain and discard the body regardless of status; never retain or log it.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	n.logger.Info("cleanup notification delivered",
		"trigger", ev.Trigger,
		"success", ev.Success,
	)
	return nil
}

// Test delivers a synthetic success event so an operator can verify the target
// (auth header, reachability) before enabling real notifications. It returns the
// same errors as Deliver (including ErrDisabled).
func (n *Notifier) Test(ctx context.Context) error {
	now := n.clock()
	return n.Deliver(ctx, Event{
		Trigger:    TriggerTest,
		StartedAt:  now.Add(-time.Second),
		FinishedAt: now,
		Success:    true,
		Inspected:  1,
		Retained:   1,
	})
}

// payload is the JSON shape sent to the webhook partner. Field names and the
// nested structure are additive; counts are ints so a partner with zero
// tolerance for numeric strings can parse them directly.
type payload struct {
	Type       string   `json:"type"`
	Trigger    string   `json:"trigger"`
	Outcome    string   `json:"outcome"`
	StartedAt  string   `json:"startedAt,omitempty"`
	FinishedAt string   `json:"finishedAt,omitempty"`
	Counts     counts   `json:"counts"`
	Errors     []string `json:"errors,omitempty"`
}

// counts mirrors the cleanup result's aggregate numbers.
type counts struct {
	Inspected     int `json:"inspected"`
	Retained      int `json:"retained"`
	LocalDeleted  int `json:"localDeleted"`
	DriveDeleted  int `json:"driveDeleted"`
	OrphanDeleted int `json:"orphanDeleted"`
	Skipped       int `json:"skipped"`
	Missing       int `json:"missing"`
}

func (n *Notifier) payload(ev Event) payload {
	outcome := "success"
	if !ev.Success {
		outcome = "failure"
	}
	p := payload{
		Type:       "cleanup",
		Trigger:    ev.Trigger,
		Outcome:    outcome,
		StartedAt:  ev.StartedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt: ev.FinishedAt.UTC().Format(time.RFC3339Nano),
		Counts: counts{
			Inspected:     ev.Inspected,
			Retained:      ev.Retained,
			LocalDeleted:  ev.LocalDeleted,
			DriveDeleted:  ev.DriveDeleted,
			OrphanDeleted: ev.OrphanDeleted,
			Skipped:       ev.Skipped,
			Missing:       ev.Missing,
		},
	}
	for i, msg := range ev.Errors {
		if i >= maxErrors {
			break
		}
		if len(msg) > maxErrorLen {
			msg = msg[:maxErrorLen]
		}
		p.Errors = append(p.Errors, msg)
	}
	return p
}

// Version is reported to webhook partners via the User-Agent header. It is a
// variable so tests and downstream tooling can set it.
var Version = "devel"

// checkDestination rejects a webhook URL whose host can only reach forbidden
// addresses. It is the SSRF guard: it runs once per delivery and again for
// every redirect hop. IP-literal hosts are checked directly; hostnames are
// resolved first so decimal/hex-encoded or DNS-backed forbidden addresses are
// caught too.
//
// Policy: unspecified, multicast, and link-local destinations (which include
// the cloud-metadata endpoint 169.254.169.254 and IPv6 link-locals) are never
// legitimate webhook targets and are blocked. Loopback and private unicast
// addresses remain allowed because local webhooks are a supported use case and
// GitSafe's API surface is loopback-bound anyway. Resolving here and dialing
// later leaves a DNS-rebinding TOCTOU window; pinning the dial would need a
// custom DialContext and is documented as deferred in PLAN.md.
func checkDestination(ctx context.Context, u *url.URL, resolveHost func(ctx context.Context, host string) ([]netip.Addr, error)) error {
	host := u.Hostname()
	if host == "" {
		return errors.New("webhook destination has no host")
	}

	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		if resolveHost == nil {
			resolveHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			}
		}
		resolved, err := resolveHost(ctx, host)
		if err != nil {
			return fmt.Errorf("webhook destination could not be resolved: %w", err)
		}
		addrs = resolved
	}

	for _, a := range addrs {
		if !isBlockedWebhookAddr(a.Unmap()) {
			return nil
		}
	}
	return webhookDestinationBlocked
}

// isBlockedWebhookAddr reports whether an address belongs to a range that is
// never a legitimate webhook target. The concrete checks deliberately avoid
// manual prefix tables: the netip classifiers cover IPv4 and IPv6 forms, and
// 4-in-6 addresses are unmapped before this is consulted.
func isBlockedWebhookAddr(a netip.Addr) bool {
	return a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast()
}

// checkRedirectDestination applies the same scheme allowlist and SSRF guard to a
// redirected URL.
func checkRedirectDestination(ctx context.Context, u *url.URL, resolveHost func(ctx context.Context, host string) ([]netip.Addr, error)) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("webhook redirect uses unsupported scheme %q", u.Scheme)
	}
	return checkDestination(ctx, u, resolveHost)
}

// redactURL strips userinfo, query, and fragment from a URL so it is safe to
// embed in error messages, history, and API responses. On a parse failure the
// raw string is returned length-bounded.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		if len(raw) > 512 {
			raw = raw[:512]
		}
		return raw
	}
	return u.Scheme + "://" + u.Host
}

// sanitizeWebhookError rewrites an *http.Client error so the raw destination
// URL (whose query string or path can carry secrets) never reaches logs,
// history, or API responses while the underlying cause (dial/TLS/timeout/…)
// stays visible alongside a redacted scheme://host.
func sanitizeWebhookError(err error, rawURL string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	redacted := redactURL(rawURL)
	if redacted != rawURL {
		msg = strings.ReplaceAll(msg, rawURL, redacted)
	}
	return msg
}
