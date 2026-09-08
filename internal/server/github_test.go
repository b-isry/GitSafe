package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

type fakeStateStore struct {
	mu     sync.Mutex
	gh     state.GitHubConnection
	hasGh  bool
	saved  bool
	errors map[string]error

	protected []state.ProtectedRepo
	jobs      []state.BackupJob
	records   []state.BackupRecord
}

func (f *fakeStateStore) GitHubConnection() (state.GitHubConnection, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gh, f.hasGh
}
func (f *fakeStateStore) SetGitHubConnection(c state.GitHubConnection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gh = c
	f.hasGh = true
}
func (f *fakeStateStore) ClearGitHubConnection() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hasGh = false
}

func (f *fakeStateStore) BackupJobs() []state.BackupJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]state.BackupJob, len(f.jobs))
	copy(out, f.jobs)
	return out
}

func (f *fakeStateStore) CreateBackupJob(j state.BackupJob) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, j)
}

func (f *fakeStateStore) UpdateBackupJob(j state.BackupJob) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.jobs {
		if f.jobs[i].ID == j.ID {
			f.jobs[i] = j
			return nil
		}
	}
	return state.ErrNotFound
}

func (f *fakeStateStore) RemoveBackupJob(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.jobs {
		if f.jobs[i].ID == id {
			f.jobs = append(f.jobs[:i], f.jobs[i+1:]...)
			return nil
		}
	}
	return state.ErrNotFound
}

func (f *fakeStateStore) BackupJob(id string) (state.BackupJob, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, j := range f.jobs {
		if j.ID == id {
			return j, true
		}
	}
	return state.BackupJob{}, false
}

func (f *fakeStateStore) UnfinishedJobs() []state.BackupJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []state.BackupJob
	for _, j := range f.jobs {
		if !state.IsTerminalState(j.State) {
			out = append(out, j)
		}
	}
	return out
}

func (f *fakeStateStore) AddBackupRecord(rec state.BackupRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
}

func (f *fakeStateStore) UpdateBackupRecord(rec state.BackupRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.errors["updateRecord"]; e != nil {
		return e
	}
	for i := range f.records {
		if f.records[i].ID == rec.ID {
			f.records[i] = rec
			return nil
		}
	}
	return state.ErrNotFound
}

func (f *fakeStateStore) RemoveBackupRecord(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.records {
		if f.records[i].ID == id {
			f.records = append(f.records[:i], f.records[i+1:]...)
			return nil
		}
	}
	return state.ErrNotFound
}

func (f *fakeStateStore) BackupRecords() []state.BackupRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]state.BackupRecord, len(f.records))
	copy(out, f.records)
	return out
}

func (f *fakeStateStore) BackupRecordsForRepo(protectedRepoID string) []state.BackupRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []state.BackupRecord
	for _, r := range f.records {
		if r.ProtectedRepoID == protectedRepoID {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeStateStore) ProtectedRepos() []state.ProtectedRepo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]state.ProtectedRepo, len(f.protected))
	copy(out, f.protected)
	return out
}

func (f *fakeStateStore) ProtectedRepo(id string) (state.ProtectedRepo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.protected {
		if p.ID == id {
			return p, true
		}
	}
	return state.ProtectedRepo{}, false
}

func (f *fakeStateStore) AddProtectedRepo(r state.ProtectedRepo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.protected == nil {
		f.protected = []state.ProtectedRepo{}
	}
	for _, existing := range f.protected {
		if existing.GitHubID == r.GitHubID {
			return state.ErrDuplicate
		}
	}
	f.protected = append(f.protected, r)
	return nil
}

func (f *fakeStateStore) RemoveProtectedRepo(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.protected {
		if f.protected[i].ID == id {
			f.protected = append(f.protected[:i], f.protected[i+1:]...)
			return nil
		}
	}
	return state.ErrNotFound
}

func (f *fakeStateStore) Save() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = true
	if e := f.errors["save"]; e != nil {
		return e
	}
	return nil
}

type fakeTokenStore struct {
	data map[string]string
}

func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{data: map[string]string{}}
}

func (f *fakeTokenStore) Set(ref, value string) error { f.data[ref] = value; return nil }
func (f *fakeTokenStore) Get(ref string) (string, error) {
	if v, ok := f.data[ref]; ok {
		return v, nil
	}
	return "", tokenstore.ErrNotFound
}
func (f *fakeTokenStore) Delete(ref string) error { delete(f.data, ref); return nil }

// newCloudServer builds a Server with the Phase 1 cloud wiring attached and the
// given state/token fakes. When oauth is nil, GitHub is not configured.
func newCloudServer(t *testing.T, st *fakeStateStore, tk *fakeTokenStore, oauth *GitHubOAuth) *Server {
	t.Helper()
	s := newTestApp(t, t.TempDir(), t.TempDir())
	if st == nil {
		st = &fakeStateStore{}
	}
	if tk == nil {
		tk = newFakeTokenStore()
	}
	if oauth == nil {
		// Configuring without oauth still wires state/token so handlers work.
		s.ConfigureCloud(st, tk, nil)
	} else {
		s.ConfigureCloud(st, tk, oauth)
	}
	return s
}

func request(t *testing.T, s *Server, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:55555"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func loginAndGetState(t *testing.T, s *Server) (sessionCookie *http.Cookie, stateVal string, rec *httptest.ResponseRecorder) {
	t.Helper()
	login := request(t, s, http.MethodGet, "/api/auth/github", nil)
	if login.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", login.Code)
	}
	loc := login.Header().Get("Location")
	u := mustParse(t, loc)
	stateVal = u.Query().Get("state")
	if stateVal == "" {
		t.Fatalf("no state in authorize location: %q", loc)
	}
	var cookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("no %s cookie set on login", sessionCookieName)
	}
	return cookie, stateVal, login
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestGitHubOAuthNotConfigured(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	rec := request(t, s, http.MethodGet, "/api/auth/github", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login status = %d, want 503", rec.Code)
	}
	rec = request(t, s, http.MethodGet, "/api/connections", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("connections status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	gh := body["github"].(map[string]any)
	if gh["configured"] != false || gh["connected"] != false {
		t.Fatalf("connections github = %+v", gh)
	}
}

func TestAppConnectionsReflectsNotConnected(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodGet, "/api/connections", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	gh := body["github"].(map[string]any)
	if gh["configured"] != true || gh["connected"] != false {
		t.Fatalf("github = %+v", gh)
	}
}

func TestGitHubCallbackMissingSession(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodGet, "/api/auth/github/callback?state=abc&code=xyz", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestGitHubCallbackMismatchedState(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, _, _ := loginAndGetState(t, s)
	rec := request(t, s, http.MethodGet,
		"/api/auth/github/callback?state=WRONG&code=xyz", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (mismatched state)", rec.Code)
	}
}

func TestGitHubCallbackMissingCode(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, stateVal, _ := loginAndGetState(t, s)
	// Empty code simulates a declined/errored OAuth return.
	rec := request(t, s, http.MethodGet, "/api/auth/github/callback?state="+stateVal+"&code=", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing code)", rec.Code)
	}
}

func TestGitHubCallbackSuccessPersists(t *testing.T) {
	st := &fakeStateStore{}
	tk := newFakeTokenStore()
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id", ClientSecret: "sec"})
	s.connectGitHub = func(ctx context.Context, code string) (string, []string, providers.Identity, error) {
		return "tok-999", []string{providers.ScopeRepo}, providers.Identity{
			ID: 42, Login: "octocat", Name: "Oc ToCat",
			AvatarURL: "https://avatar/x.png", Scopes: []string{providers.ScopeRepo},
		}, nil
	}
	cookie, stateVal, _ := loginAndGetState(t, s)
	rec := request(t, s, http.MethodGet,
		"/api/auth/github/callback?state="+stateVal+"&code=the-code", cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", rec.Code)
	}
	if !st.hasGh {
		t.Fatal("expected GitHub connection to be persisted")
	}
	conn, ok := st.GitHubConnection()
	if !ok {
		t.Fatal("GitHubConnection() reports not connected")
	}
	if conn.GitHubID != 42 || conn.Login != "octocat" || conn.Name != "Oc ToCat" {
		t.Fatalf("identity not persisted: %+v", conn)
	}
	if conn.TokenRef != tokenstore.GitHubToken {
		t.Fatalf("tokenRef = %q, want %q", conn.TokenRef, tokenstore.GitHubToken)
	}
	if !slices.Contains(conn.Scopes, providers.ScopeRepo) {
		t.Fatalf("repo scope not persisted: %v", conn.Scopes)
	}
	if !st.saved {
		t.Fatal("expected state to be saved after connect")
	}
	if tk.data[tokenstore.GitHubToken] != "tok-999" {
		t.Fatalf("token not stored in keychain: %q", tk.data[tokenstore.GitHubToken])
	}
}

func TestGitHubCallbackMissingRepoScope(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	s.connectGitHub = func(ctx context.Context, code string) (string, []string, providers.Identity, error) {
		// Token granted but WITHOUT the repo scope must be rejected.
		return "tok", nil, providers.Identity{ID: 1, Login: "l", Scopes: []string{"public_repo"}}, nil
	}
	cookie, stateVal, _ := loginAndGetState(t, s)
	rec := request(t, s, http.MethodGet,
		"/api/auth/github/callback?state="+stateVal+"&code=c", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing repo scope)", rec.Code)
	}
	st := s.stateStore.(*fakeStateStore)
	if st.hasGh {
		t.Fatal("connection must not persist without repo scope")
	}
}

func TestGitHubCallbackStateSingleUse(t *testing.T) {
	st := &fakeStateStore{}
	tk := newFakeTokenStore()
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})
	s.connectGitHub = func(ctx context.Context, code string) (string, []string, providers.Identity, error) {
		return "tok", []string{providers.ScopeRepo}, providers.Identity{ID: 2, Login: "u", Scopes: []string{providers.ScopeRepo}}, nil
	}
	cookie, stateVal, _ := loginAndGetState(t, s)
	path := "/api/auth/github/callback?state=" + stateVal + "&code=c"
	rec := request(t, s, http.MethodGet, path, cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("first callback status = %d, want 302", rec.Code)
	}
	// Replaying the same state must be rejected.
	rec2 := request(t, s, http.MethodGet, path, cookie)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback status = %d, want 400", rec2.Code)
	}
}

func TestGitHubDisconnectClearsConnection(t *testing.T) {
	st := &fakeStateStore{}
	tk := newFakeTokenStore()
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})
	tk.data[tokenstore.GitHubToken] = "tok"
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})

	// Establish a session and fetch a CSRF token.
	csrf := request(t, s, http.MethodGet, "/api/csrf", nil)
	var csrfBody map[string]string
	_ = json.Unmarshal(csrf.Body.Bytes(), &csrfBody)
	var cookie *http.Cookie
	for _, c := range csrf.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie from /api/csrf")
	}

	// Attempt without a CSRF token → rejected.
	noToken := request(t, s, http.MethodDelete, "/api/connections/github", cookie)
	if noToken.Code != http.StatusForbidden {
		t.Fatalf("disconnect without CSRF status = %d, want 403", noToken.Code)
	}
	if !st.hasGh {
		t.Fatal("connection must not be cleared without valid CSRF")
	}

	// With a valid CSRF token → disconnected.
	req := httptest.NewRequest(http.MethodDelete, "/api/connections/github", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrfBody["csrfToken"])
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect status = %d, want 200", rec.Code)
	}
	if st.hasGh {
		t.Fatal("expected GitHub connection cleared")
	}
	if _, ok := tk.data[tokenstore.GitHubToken]; ok {
		t.Fatal("expected token removed from keychain")
	}
}

func TestGitHubDisconnectNoSession(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodDelete, "/api/connections/github", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRepositoriesDisconnected(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodGet, "/api/repositories", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestRepositoriesMissingToken(t *testing.T) {
	st := &fakeStateStore{}
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})
	// Token store empty → missing token.
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodGet, "/api/repositories", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
