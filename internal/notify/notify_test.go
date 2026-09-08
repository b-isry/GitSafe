package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/config"
)

func testNotifier(cfg config.NotificationConfig) *Notifier {
	return New(func() config.NotificationConfig { return cfg }, nil)
}

func TestDeliverDisabledNoRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testNotifier(config.NotificationConfig{Enabled: false})
	err := n.Deliver(context.Background(), Event{Trigger: TriggerManual, Success: true})
	if !errorsIs(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("disabled notifier made %d requests", calls.Load())
	}
}

func TestDeliverSuccessPayloadAndAuth(t *testing.T) {
	type countsDTO struct {
		Inspected     int `json:"inspected"`
		Retained      int `json:"retained"`
		LocalDeleted  int `json:"localDeleted"`
		DriveDeleted  int `json:"driveDeleted"`
		OrphanDeleted int `json:"orphanDeleted"`
		Skipped       int `json:"skipped"`
		Missing       int `json:"missing"`
	}
	type payloadDTO struct {
		Type       string    `json:"type"`
		Trigger    string    `json:"trigger"`
		Outcome    string    `json:"outcome"`
		StartedAt  string    `json:"startedAt"`
		FinishedAt string    `json:"finishedAt"`
		Counts     countsDTO `json:"counts"`
		Errors     []string  `json:"errors"`
	}

	var got payloadDTO
	var gotAuth, gotContentType, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Test-Token")
		gotContentType = r.Header.Get("Content-Type")
		gotUA = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testNotifier(config.NotificationConfig{
		Enabled:               true,
		Type:                  "webhook",
		WebhookURL:            srv.URL,
		WebhookAuthHeader:     "X-Test-Token",
		WebhookAuthToken:      "hunter2",
		NotifyOnSuccess:       true,
		NotifyOnFailure:       true,
		WebhookTimeoutSeconds: 5,
	})
	Version = "gitsafe-test"

	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	ev := Event{
		Trigger:       TriggerScheduled,
		StartedAt:     start,
		FinishedAt:    start.Add(time.Minute),
		Success:       true,
		Inspected:     5,
		Retained:      3,
		LocalDeleted:  2,
		DriveDeleted:  1,
		OrphanDeleted: 1,
		Skipped:       0,
		Missing:       0,
	}
	if err := n.Deliver(context.Background(), ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if gotAuth != "hunter2" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}
	if gotUA != "GitSafe/gitsafe-test" {
		t.Errorf("user-agent = %q", gotUA)
	}
	if got.Type != "cleanup" || got.Trigger != "scheduled" || got.Outcome != "success" {
		t.Errorf("payload discriminator wrong: %+v", got)
	}
	if got.Counts.Inspected != 5 || got.Counts.Retained != 3 || got.Counts.LocalDeleted != 2 ||
		got.Counts.DriveDeleted != 1 || got.Counts.OrphanDeleted != 1 {
		t.Errorf("payload counts wrong: %+v", got.Counts)
	}
	if got.StartedAt == "" || got.FinishedAt == "" {
		t.Errorf("payload times missing: %+v", got)
	}
}

func TestDeliverFailureOutcomeAndStatusError(t *testing.T) {
	var gotTrigger, gotOutcome string
	var gotErrors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode: %v", err)
		}
		gotTrigger, gotOutcome = p.Trigger, p.Outcome
		gotErrors = p.Errors
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook", WebhookURL: srv.URL})
	err := n.Deliver(context.Background(), Event{
		Trigger: TriggerManual,
		Success: false,
		Errors:  []string{"delete drive file \"abc\": boom"},
	})
	if err == nil {
		t.Fatal("expected delivery error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error should mention status: %v", err)
	}
	if gotTrigger != TriggerManual || gotOutcome != "failure" {
		t.Errorf("failure payload wrong: trigger=%q outcome=%q", gotTrigger, gotOutcome)
	}
	if len(gotErrors) != 1 || gotErrors[0] != "delete drive file \"abc\": boom" {
		t.Errorf("errors not embedded: %v", gotErrors)
	}
}

func TestDeliverExpandsEnvReferencedAuthToken(t *testing.T) {
	t.Setenv("GITSAFE_WEBHOOK_TOKEN", "resolved-from-env")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Test-Token")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testNotifier(config.NotificationConfig{
		Enabled:           true,
		Type:              "webhook",
		WebhookURL:        srv.URL,
		WebhookAuthHeader: "X-Test-Token",
		WebhookAuthToken:  "${GITSAFE_WEBHOOK_TOKEN}",
	})
	if err := n.Deliver(context.Background(), Event{Success: true}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if gotAuth != "resolved-from-env" {
		t.Errorf("auth header = %q, want env-resolved value", gotAuth)
	}
}

func TestDeliverAuthTokenNeverLeaksInError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	n := testNotifier(config.NotificationConfig{
		Enabled:           true,
		Type:              "webhook",
		WebhookURL:        srv.URL,
		WebhookAuthHeader: "Authorization",
		WebhookAuthToken:  "ultra-secret-token",
	})
	err := n.Deliver(context.Background(), Event{Success: false})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "ultra-secret-token") {
		t.Errorf("token leaked into error: %v", err)
	}
}

func TestDeliverTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer srv.Close()

	n := testNotifier(config.NotificationConfig{
		Enabled:               true,
		Type:                  "webhook",
		WebhookURL:            srv.URL,
		WebhookTimeoutSeconds: 1,
	})
	start := time.Now()
	err := n.Deliver(context.Background(), Event{Success: true})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("delivery took too long: %v", time.Since(start))
	}
}

func TestDeliverRejectsInvalidConfigDefensively(t *testing.T) {
	n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook"}) // no URL
	err := n.Deliver(context.Background(), Event{Success: true})
	if err == nil {
		t.Fatal("expected error for missing webhook url")
	}
	if errorsIs(err, ErrDisabled) {
		t.Fatalf("invalid config must not look disabled: %v", err)
	}
}

func TestDeliverTimeoutSecondsZeroUsesDefault(t *testing.T) {
	n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook"})
	settings, err := n.resolve()
	if err == nil {
		t.Fatalf("expected missing-url error, got %+v", settings)
	}

	n = testNotifier(config.NotificationConfig{
		Enabled:    true,
		Type:       "webhook",
		WebhookURL: "https://example.invalid/hook",
	})
	settings, err = n.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if settings.Timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", settings.Timeout, DefaultTimeout)
	}
}

func TestTestDeliversSyntheticEvent(t *testing.T) {
	var got payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook", WebhookURL: srv.URL})
	if err := n.Test(context.Background()); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if got.Trigger != TriggerTest || got.Outcome != "success" || got.Counts.Inspected != 1 {
		t.Errorf("synthetic payload wrong: %+v", got)
	}
}

func TestDeliverErrorsCapped(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode: %v", err)
		}
		got = p.Errors
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook", WebhookURL: srv.URL})
	errs := make([]string, maxErrors*2)
	for i := range errs {
		errs[i] = strings.Repeat("x", maxErrorLen+100)
	}
	if err := n.Deliver(context.Background(), Event{Success: false, Errors: errs}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(got) != maxErrors {
		t.Errorf("errors delivered = %d, want %d", len(got), maxErrors)
	}
	for _, e := range got {
		if len(e) > maxErrorLen {
			t.Errorf("error message not truncated: len=%d", len(e))
		}
	}
}

func TestDeliverConcurrency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ignored body"))
	}))
	defer srv.Close()

	n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook", WebhookURL: srv.URL})
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			done <- n.Deliver(context.Background(), Event{Success: true})
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent Deliver: %v", err)
		}
	}
}

func errorsIs(err, target error) bool { return errors.Is(err, target) }

// --- SSRF / webhook destination security (Phase 9) ---

func TestDeliverBlocksForbiddenDestinations(t *testing.T) {
	urls := []string{
		"http://169.254.169.254/latest/meta-data/", // cloud metadata endpoint
		"http://169.254.0.1/",                      // IPv4 link-local
		"http://[fe80::1]/",                        // IPv6 link-local
		"http://0.0.0.0/",                          // unspecified
		"http://[::]/",                             // unspecified IPv6
		"http://224.0.0.1/",                        // IPv4 multicast
		"http://[ff02::1]/",                        // IPv6 link-local multicast
	}
	for _, u := range urls {
		n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook", WebhookURL: u})
		err := n.Deliver(context.Background(), Event{Success: true})
		if err == nil {
			t.Errorf("Deliver(%q): expected blocked error", u)
			continue
		}
		if errorsIs(err, ErrDisabled) {
			t.Errorf("Deliver(%q): must not look disabled: %v", u, err)
		}
		if !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("Deliver(%q): unexpected error: %v", u, err)
		}
	}
}

func TestDeliverLiteralDestinationDoesNotResolve(t *testing.T) {
	var calls atomic.Int32
	n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook", WebhookURL: "http://169.254.169.254/x"})
	n.resolveHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
		calls.Add(1)
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	if err := n.Deliver(context.Background(), Event{Success: true}); err == nil {
		t.Fatal("expected blocked error")
	}
	if calls.Load() != 0 {
		t.Errorf("resolver invoked %d times for an IP-literal destination", calls.Load())
	}
}

func TestCheckDestinationAllowsLegitimateTargets(t *testing.T) {
	ctx := context.Background()
	// Allow: loopback, IPv6 loopback, RFC1918/ULA private unicast, and a
	// public hostname that resolves to public unicast addresses.
	literals := []string{
		"http://127.0.0.1:9000/hook",
		"http://[::1]:9000/hook",
		"http://10.0.0.5/hook",
		"http://172.16.0.1/hook",
		"http://192.168.1.10/hook",
	}
	for _, raw := range literals {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if err := checkDestination(ctx, u, nil); err != nil {
			t.Errorf("checkDestination(%q): unexpected error: %v", raw, err)
		}
	}

	u, err := url.Parse("https://hooks.example.com/xyz")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolver := func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	if err := checkDestination(ctx, u, resolver); err != nil {
		t.Errorf("checkDestination(hostname): unexpected error: %v", err)
	}
}

func TestDeliverBlocksHostnameResolvingForbidden(t *testing.T) {
	metadata := netip.MustParseAddr("169.254.169.254")
	for _, raw := range []string{
		"http://metadata.internal/latest/meta-data/", // DNS-backed trick
		"http://2852039166/",                         // decimal-encoded 169.254.169.254
	} {
		n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook", WebhookURL: raw})
		n.resolveHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{metadata}, nil
		}
		err := n.Deliver(context.Background(), Event{Success: true})
		if err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("Deliver(%q): expected blocked error, got %v", raw, err)
		}
	}
}

func TestDeliverBlocksMappedIPv6LinkLocal(t *testing.T) {
	n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook", WebhookURL: "http://[::ffff:169.254.169.254]/x"})
	err := n.Deliver(context.Background(), Event{Success: true})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected 4-in-6 metadata address to be blocked, got %v", err)
	}
}

func TestDeliverRedirectToForbiddenBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook", WebhookURL: srv.URL})
	err := n.Deliver(context.Background(), Event{Success: true})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected blocked redirect error, got %v", err)
	}
}

func TestDeliverRedirectLoopCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()

	n := testNotifier(config.NotificationConfig{Enabled: true, Type: "webhook", WebhookURL: srv.URL})
	err := n.Deliver(context.Background(), Event{Success: true})
	if err == nil || !strings.Contains(err.Error(), "redirected more than 5 times") {
		t.Fatalf("expected redirect cap error, got %v", err)
	}
}

func TestDeliverErrorRedactsURLSecrets(t *testing.T) {
	const secret = "super-s3cret-query-value"
	n := testNotifier(config.NotificationConfig{
		Enabled:    true,
		Type:       "webhook",
		WebhookURL: "http://127.0.0.1:1/hook?secret=" + secret,
	})
	err := n.Deliver(context.Background(), Event{Success: true})
	if err == nil {
		t.Fatal("expected a delivery error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("query secret leaked into error: %v", err)
	}
	if strings.Contains(err.Error(), "secret=") {
		t.Errorf("query string leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("sanitized error should keep the redacted host: %v", err)
	}
}
