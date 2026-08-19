package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/server/api"
)

// keyedRequest sends one request with an API key attached, and reports the
// progress. Everything about scopes is a status code, so nothing else is read.
func keyedRequest(t *testing.T, method, url, key string, body any) int {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func authServer(t *testing.T, svc *fakeService, keys ...string) string {
	t.Helper()
	cfg := &config.ServerConfig{Auth: config.AuthConfig{Type: "api_key", APIKeys: keys}}
	return newTestServer(t, svc, cfg).URL
}

func repoService() *fakeService {
	return &fakeService{repos: map[string]*domain.Repo{"r1": {ID: "r1", Name: "orders"}}}
}

// TestReadScopeCannotMutate is the reason scopes exist: a retrieval client acts
// for a language model, and no prompt it is given may reach a DELETE.
func TestReadScopeCannotMutate(t *testing.T) {
	mutations := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"delete repo", http.MethodDelete, "/api/v1/repos/r1", nil},
		{"add repo", http.MethodPost, "/api/v1/repos", map[string]any{"name": "x", "source": "local", "path": "/tmp/x"}},
		{"index", http.MethodPost, "/api/v1/repos/r1/index", map[string]any{}},
		{"reset", http.MethodPost, "/api/v1/repos/r1/reset", nil},
		{"push commits", http.MethodPost, "/api/v1/repos/r1/commits", map[string]any{"commits": []any{}}},
		{"ingest otel", http.MethodPost, "/api/v1/otel/service-graph", map[string]any{"edges": []any{}}},
		{"compact", http.MethodPost, "/api/v1/admin/compact", nil},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			svc := repoService()
			base := authServer(t, svc, "read:reader", "admin:root")

			if got := keyedRequest(t, tt.method, base+tt.path, "reader", tt.body); got != http.StatusForbidden {
				t.Errorf("read key: status = %d, want 403", got)
			}
			if _, alive := svc.repos["r1"]; !alive {
				t.Error("the read key deleted the repository anyway")
			}
			if svc.compacted {
				t.Error("the read key compacted the index anyway")
			}
			if got := keyedRequest(t, tt.method, base+tt.path, "root", tt.body); got == http.StatusForbidden {
				t.Errorf("admin key: status = 403, want the route to run")
			}
		})
	}
}

func TestReadScopeKeepsEveryRetrievalRoute(t *testing.T) {
	reads := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/api/v1/search", map[string]any{"query": "handler"}},
		{http.MethodPost, "/api/v1/context", map[string]any{"query": "handler"}},
		{http.MethodPost, "/api/v1/nav/symbol", map[string]any{"name": "Handle"}},
		{http.MethodPost, "/api/v1/graph/neighbors", map[string]any{"unit_id": "u1"}},
		{http.MethodGet, "/api/v1/repos", nil},
		{http.MethodGet, "/api/v1/repos/r1", nil},
		{http.MethodGet, "/api/v1/repos/r1/jobs", nil},
		{http.MethodGet, "/api/v1/services", nil},
		{http.MethodGet, "/api/v1/topics", nil},
		{http.MethodGet, "/api/v1/stats", nil},
	}
	base := authServer(t, repoService(), "read:reader")
	for _, tt := range reads {
		if got := keyedRequest(t, tt.method, base+tt.path, "reader", tt.body); got != http.StatusOK {
			t.Errorf("%s %s: status = %d, want 200", tt.method, tt.path, got)
		}
	}
}

// TestUnprefixedKeyKeepsFullAccess is the migration path: a config written
// before scopes existed grants exactly what it granted then.
func TestUnprefixedKeyKeepsFullAccess(t *testing.T) {
	svc := repoService()
	base := authServer(t, svc, "legacy-secret")

	if got := keyedRequest(t, http.MethodDelete, base+"/api/v1/repos/r1", "legacy-secret", nil); got != http.StatusNoContent {
		t.Errorf("status = %d, want 204: an existing single-key config must keep working", got)
	}
	if got := keyedRequest(t, http.MethodPost, base+"/api/v1/search", "legacy-secret",
		map[string]any{"query": "handler"}); got != http.StatusOK {
		t.Errorf("search with a legacy key: status = %d, want 200", got)
	}
}

// TestKeyWithColonIsNotAScopePrefix: only the two known prefixes name a scope,
// so a key that merely contains a colon is still the key it always was.
func TestKeyWithColonIsNotAScopePrefix(t *testing.T) {
	base := authServer(t, repoService(), "team:orders:secret")

	if got := keyedRequest(t, http.MethodDelete, base+"/api/v1/repos/r1", "team:orders:secret", nil); got != http.StatusNoContent {
		t.Errorf("status = %d, want 204: the whole string is the key", got)
	}
}

func TestScopePrefixIsNotPartOfTheCredential(t *testing.T) {
	base := authServer(t, repoService(), "read:reader")

	if got := keyedRequest(t, http.MethodPost, base+"/api/v1/search", "read:reader",
		map[string]any{"query": "handler"}); got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: the client sends the key, not the prefix", got)
	}
}

func TestUnknownKeyIsStillUnauthorized(t *testing.T) {
	base := authServer(t, repoService(), "read:reader", "admin:root")

	if got := keyedRequest(t, http.MethodPost, base+"/api/v1/search", "nope",
		map[string]any{"query": "handler"}); got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
	if got := keyedRequest(t, http.MethodPost, base+"/api/v1/search", "",
		map[string]any{"query": "handler"}); got != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", got)
	}
}

// TestEmptyKeyAfterPrefixIsNotRegistered: "read:" with nothing behind it is a
// typo, and must not mint a key that authenticates whoever sends that blank.
func TestEmptyKeyAfterPrefixIsNotRegistered(t *testing.T) {
	base := authServer(t, repoService(), "read:", "admin:root")

	if got := keyedRequest(t, http.MethodPost, base+"/api/v1/search", "",
		map[string]any{"query": "handler"}); got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
}

// TestScopesDoNotGateAnUnauthenticatedServer: with auth off there are no keys
// to carry a scope, and a check that failed closed would take away routes such
// a deployment has always served — the eval harness runs exactly this way.
func TestScopesDoNotGateAnUnauthenticatedServer(t *testing.T) {
	svc := repoService()
	srv := newTestServer(t, svc, nil)

	if got := keyedRequest(t, http.MethodDelete, srv.URL+"/api/v1/repos/r1", "", nil); got != http.StatusNoContent {
		t.Errorf("status = %d, want 204 with auth disabled", got)
	}
	if got := keyedRequest(t, http.MethodPost, srv.URL+"/api/v1/admin/compact", "", nil); got != http.StatusOK {
		t.Errorf("compact: status = %d, want 200 with auth disabled", got)
	}
}

func TestForbiddenBodyCarriesTheCode(t *testing.T) {
	base := authServer(t, repoService(), "read:reader")

	req, err := http.NewRequest(http.MethodDelete, base+"/api/v1/repos/r1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-API-Key", "reader")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()

	var body api.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != api.CodeForbidden {
		t.Errorf("code = %q, want %q", body.Code, api.CodeForbidden)
	}
}
