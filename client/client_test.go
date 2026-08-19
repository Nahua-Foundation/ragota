package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/client"
	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/server/api"
	"github.com/Nahua-Foundation/ragota/internal/server/bootstrap"
	"github.com/Nahua-Foundation/ragota/internal/servertest"
)

// The client exists to encode a contract, so it is checked against the server
// that serves it: every call below goes through the real api.Server router over
// a real socket. A test against hand-written JSON would agree with whatever the
// client sends and prove nothing about the other side.

// indexWait bounds the wait for the fixture repository to finish index.
const indexWait = 60 * time.Second

// newClient starts a server with no authentication configured and returns a
// client for it.
func newClient(t *testing.T) *client.Client {
	t.Helper()
	srv, _ := servertest.SetupServer(t)
	return client.New(srv.URL)
}

// newAuthedServer starts a server that requires an API key. The keys are given
// in their configured form, scope prefix and all — what a client sends is the
// part behind the prefix.
func newAuthedServer(t *testing.T, keys ...string) string {
	t.Helper()
	t.Setenv("RAGOTA_BM25_PATH", t.TempDir())

	cfg := servertest.TestConfig(t)
	cfg.Server.Auth = config.AuthConfig{Type: "api_key", APIKeys: keys}

	svc, err := bootstrap.Build(context.Background(), cfg)
	if err != nil {
		t.Fatalf("setup build: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })

	srv := api.NewServer(svc, &cfg.Server)
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return ts.URL
}

// indexFixture registers testdata/go-test-project and waits for the pass to
// finish, so that the retrieval calls below have something to find.
func indexFixture(t *testing.T, ctx context.Context, c *client.Client) *client.Repo {
	t.Helper()
	repo, err := c.AddRepo(ctx, &client.AddRepoRequest{
		Name:   "calculator",
		Source: "local",
		Path:   servertest.TestdataPath(t, "go-test-project"),
	})
	if err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	ack, err := c.Index(ctx, repo.ID, &client.IndexRequest{Force: true})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if ack.Status == "" {
		t.Error("IndexAck carries no status")
	}

	deadline := time.Now().Add(indexWait)
	for {
		got, err := c.GetRepo(ctx, repo.ID)
		if err != nil {
			t.Fatalf("GetRepo: %v", err)
		}
		if got.Status != "indexing" {
			if got.Status != "idle" {
				t.Fatalf("repo finished as %q: %s", got.Status, got.LastError)
			}
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("repository still indexing after %s", indexWait)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// unitID finds an indexed symbol by name and returns its id, which is the only
// way to name a unit to the graph endpoints.
func unitID(t *testing.T, ctx context.Context, c *client.Client, repoID, name string) string {
	t.Helper()
	res, err := c.Symbol(ctx, &client.SymbolRequest{RepoID: repoID, Symbol: name})
	if err != nil {
		t.Fatalf("Symbol(%q): %v", name, err)
	}
	for _, s := range res.Symbols {
		if s.Name == name {
			return s.ID
		}
	}
	t.Fatalf("no symbol named %q among %d results", name, len(res.Symbols))
	return ""
}

func TestClientAgainstRealServer(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)

	health, err := c.CheckCompatibility(ctx)
	if err != nil {
		t.Fatalf("CheckCompatibility: %v", err)
	}
	if health.APIVersion != client.SchemaVersion {
		t.Errorf("server api_version = %q, client speaks %q", health.APIVersion, client.SchemaVersion)
	}
	if health.Status != "ok" || health.Version == "" {
		t.Errorf("health = %+v", health)
	}

	repo := indexFixture(t, ctx, c)

	t.Run("Repos", func(t *testing.T) {
		list, err := c.ListRepos(ctx)
		if err != nil {
			t.Fatalf("ListRepos: %v", err)
		}
		var found bool
		for _, r := range list {
			found = found || r.ID == repo.ID
		}
		if !found {
			t.Fatalf("repo %s missing from the listing", repo.ID)
		}
		if list[0].Name == "" || list[0].Path == "" {
			t.Errorf("repo fields did not decode: %+v", list[0])
		}

		state, err := c.SyncState(ctx, repo.ID)
		if err != nil {
			t.Fatalf("SyncState: %v", err)
		}
		if state.RepoID != repo.ID {
			t.Errorf("sync state repo_id = %q, want %q", state.RepoID, repo.ID)
		}
		if state.IndexedAt == 0 {
			t.Error("sync state reports the repo was never indexed")
		}

		if _, err := c.Coverage(ctx, repo.ID); err != nil {
			t.Fatalf("Coverage: %v", err)
		}

		jobs, err := c.Jobs(ctx, repo.ID, 10)
		if err != nil {
			t.Fatalf("Jobs: %v", err)
		}
		if jobs.Total != len(jobs.Jobs) {
			t.Errorf("total %d does not count the %d jobs returned", jobs.Total, len(jobs.Jobs))
		}
	})

	t.Run("Search", func(t *testing.T) {
		res, err := c.Search(ctx, &client.SearchRequest{
			Query: "Add", Mode: "keyword", Repos: []string{repo.ID}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(res.Hits) == 0 {
			t.Fatal("no hits for a term the fixture defines")
		}
		hit := res.Hits[0]
		if hit.RepoID != repo.ID || !strings.HasSuffix(hit.FilePath, ".go") || hit.Line == 0 {
			t.Errorf("hit did not decode: %+v", hit)
		}
		if res.Truncated {
			t.Error("truncated without a byte budget")
		}
	})

	t.Run("SearchSnippetModes", func(t *testing.T) {
		none, err := c.Search(ctx, &client.SearchRequest{
			Query: "Add", Mode: "keyword", Repos: []string{repo.ID}, Limit: 5,
			Snippet: client.SnippetNone,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		for _, h := range none.Hits {
			if h.Snippet != "" {
				t.Errorf("snippet=none still carried a body: %q", h.Snippet)
			}
		}

		line, err := c.Search(ctx, &client.SearchRequest{
			Query: "Add", Mode: "keyword", Repos: []string{repo.ID}, Limit: 5,
			Snippet: client.SnippetLine,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		for _, h := range line.Hits {
			if strings.Contains(h.Snippet, "\n") {
				t.Errorf("snippet=line returned more than a line: %q", h.Snippet)
			}
		}
	})

	t.Run("SearchMaxBytes", func(t *testing.T) {
		full, err := c.Search(ctx, &client.SearchRequest{
			Query: "Add", Mode: "keyword", Repos: []string{repo.ID}, Limit: 20,
			Snippet: client.SnippetNone,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(full.Hits) < 2 {
			t.Skipf("fixture yields %d hits; nothing to budget", len(full.Hits))
		}
		const budget = 400
		cut, err := c.Search(ctx, &client.SearchRequest{
			Query: "Add", Mode: "keyword", Repos: []string{repo.ID}, Limit: 20,
			Snippet: client.SnippetNone, MaxBytes: budget,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if !cut.Truncated {
			t.Fatalf("a %d byte budget did not truncate %d hits", budget, len(full.Hits))
		}
		if len(cut.Hits) >= len(full.Hits) {
			t.Errorf("truncated response kept %d of %d hits", len(cut.Hits), len(full.Hits))
		}
		if cut.Total != full.Total {
			t.Errorf("total changed under a budget: %d vs %d", cut.Total, full.Total)
		}
	})

	t.Run("Context", func(t *testing.T) {
		res, err := c.Context(ctx, &client.ContextRequest{
			Query: "Add", Mode: "keyword", Repos: []string{repo.ID}, Limit: 5, Hops: 1,
		})
		if err != nil {
			t.Fatalf("Context: %v", err)
		}
		if len(res.Items) == 0 {
			t.Fatal("no context items")
		}
		if res.Items[0].Hit == nil {
			t.Error("context item carries no hit")
		}
	})

	t.Run("Symbol", func(t *testing.T) {
		res, err := c.Symbol(ctx, &client.SymbolRequest{RepoID: repo.ID, Symbol: "Add"})
		if err != nil {
			t.Fatalf("Symbol: %v", err)
		}
		if len(res.Symbols) == 0 || res.Total != len(res.Symbols) {
			t.Fatalf("symbols = %d, total = %d", len(res.Symbols), res.Total)
		}
		var exact *client.ASTSymbol
		for _, s := range res.Symbols {
			if s.Name == "Add" {
				exact = s
				break
			}
		}
		if exact == nil {
			t.Fatal("no symbol named Add")
		}
		if exact.FilePath == "" || exact.StartLine == 0 || exact.ID == "" {
			t.Errorf("symbol did not decode: %+v", exact)
		}
	})

	t.Run("Navigation", func(t *testing.T) {
		res, err := c.Symbol(ctx, &client.SymbolRequest{RepoID: repo.ID, Name: "Add"})
		if err != nil || len(res.Symbols) == 0 {
			t.Fatalf("Symbol: %v (%d results)", err, len(res.Symbols))
		}
		sym := res.Symbols[0]

		def, err := c.Definition(ctx, &client.DefinitionRequest{
			RepoID: repo.ID, FilePath: sym.FilePath,
			Position: client.Position{Line: sym.StartLine},
		})
		if err != nil {
			t.Fatalf("Definition: %v", err)
		}
		if def.Definition == nil {
			t.Error("no definition at the line the symbol starts on")
		}

		refs, err := c.References(ctx, &client.ReferencesRequest{
			RepoID: repo.ID, FilePath: sym.FilePath,
			Position: client.Position{Line: sym.StartLine}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("References: %v", err)
		}
		if refs.Total != len(refs.References) {
			t.Errorf("total %d does not count the %d references", refs.Total, len(refs.References))
		}
	})

	t.Run("Graph", func(t *testing.T) {
		addID := unitID(t, ctx, c, repo.ID, "Add")
		mainID := unitID(t, ctx, c, repo.ID, "main")

		nb, err := c.Neighbors(ctx, &client.NeighborsRequest{UnitID: addID})
		if err != nil {
			t.Fatalf("Neighbors: %v", err)
		}
		if nb.Center == nil || nb.Center.ID != addID {
			t.Fatalf("neighbors centered on %+v", nb.Center)
		}
		if len(nb.In) == 0 {
			t.Error("Add is called by main and by its test, but has no incoming edges")
		}

		path, err := c.GraphPath(ctx, &client.GraphPathRequest{
			FromUnitID: mainID, ToUnitID: addID, MaxDepth: 3,
		})
		if err != nil {
			t.Fatalf("GraphPath: %v", err)
		}
		if path.Length != len(path.Steps) {
			t.Errorf("length %d does not count the %d steps", path.Length, len(path.Steps))
		}

		// No path is an answer, not an error: the same unit reached from
		// nothing still returns 200 with an empty list.
		none, err := c.GraphPath(ctx, &client.GraphPathRequest{
			FromUnitID: addID, ToUnitID: mainID, MaxDepth: 1,
		})
		if err != nil {
			t.Fatalf("GraphPath (no path): %v", err)
		}
		if none.Length != len(none.Steps) {
			t.Errorf("length %d does not count the %d steps", none.Length, len(none.Steps))
		}

		if _, err := c.Trace(ctx, &client.TraceRequest{
			RepoID: repo.ID, Symbol: "Add", Param: "a", MaxDepth: 2,
		}); err != nil {
			t.Fatalf("Trace: %v", err)
		}
	})

	t.Run("ServicesAndTopics", func(t *testing.T) {
		// The fixture is one Go package with no deployable in it, so these
		// answer empty. What is under test is the encoding: an empty graph
		// must decode as empty lists, not as a decode failure.
		svcs, err := c.Services(ctx, &client.ServicesRequest{Repos: []string{repo.ID}})
		if err != nil {
			t.Fatalf("Services: %v", err)
		}
		if svcs.Services == nil || svcs.Links == nil {
			t.Errorf("service lists decoded as nil: %+v", svcs)
		}

		diagram, err := c.ServicesExport(ctx, client.FormatMermaid)
		if err != nil {
			t.Fatalf("ServicesExport: %v", err)
		}
		if !strings.HasPrefix(diagram, "flowchart") {
			t.Errorf("mermaid export = %q", diagram)
		}
		if _, err := c.ServicesExport(ctx, client.FormatDOT); err != nil {
			t.Fatalf("ServicesExport(dot): %v", err)
		}

		if _, err := c.Topics(ctx, ""); err != nil {
			t.Fatalf("Topics: %v", err)
		}
	})

	t.Run("Stats", func(t *testing.T) {
		stats, err := c.Stats(ctx)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if len(stats.Indexers) == 0 {
			t.Fatal("no indexer statistics after an index pass")
		}
	})
}

func TestErrorsAreValues(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)

	t.Run("ValidationFailed", func(t *testing.T) {
		_, err := c.Search(ctx, &client.SearchRequest{})
		if !errors.Is(err, client.ErrValidationFailed) {
			t.Fatalf("empty query gave %v, want ErrValidationFailed", err)
		}
		var apiErr *client.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("error is not a *client.Error: %T", err)
		}
		if apiErr.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", apiErr.StatusCode)
		}
		if apiErr.Message == "" {
			t.Error("no human-readable message survived")
		}
		// An unrelated sentinel must not match, or the values are useless.
		if errors.Is(err, client.ErrNotFound) {
			t.Error("validation_failed matched ErrNotFound")
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		_, err := c.GetRepo(ctx, "no-such-repo")
		if !errors.Is(err, client.ErrNotFound) {
			t.Fatalf("unknown repo gave %v, want ErrNotFound", err)
		}
	})

	t.Run("SymbolNeedsASelector", func(t *testing.T) {
		_, err := c.Symbol(ctx, &client.SymbolRequest{})
		if !errors.Is(err, client.ErrValidationFailed) {
			t.Fatalf("unfiltered symbol query gave %v, want ErrValidationFailed", err)
		}
	})

	t.Run("PayloadTooLarge", func(t *testing.T) {
		_, err := c.Search(ctx, &client.SearchRequest{Query: strings.Repeat("x", 2<<20)})
		if !errors.Is(err, client.ErrPayloadTooLarge) {
			t.Fatalf("oversized body gave %v, want ErrPayloadTooLarge", err)
		}
		var apiErr *client.Error
		if errors.As(err, &apiErr) && apiErr.LimitBytes <= 0 {
			t.Errorf("413 did not report the accepted size: %+v", apiErr)
		}
	})
}

func TestScopes(t *testing.T) {
	ctx := context.Background()
	const readKey, adminKey = "read-only-key", "full-key"
	url := newAuthedServer(t, "read:"+readKey, "admin:"+adminKey)

	t.Run("NoKey", func(t *testing.T) {
		c := client.New(url)
		if _, err := c.Stats(ctx); !errors.Is(err, client.ErrUnauthorized) {
			t.Fatalf("unauthenticated call gave %v, want ErrUnauthorized", err)
		}
		// Health sits outside authentication, which is what makes it usable to
		// tell a bad key from an unreachable server.
		if _, err := c.Health(ctx); err != nil {
			t.Fatalf("Health: %v", err)
		}
	})

	t.Run("ReadKeyRetrievesButCannotMutate", func(t *testing.T) {
		c := client.New(url, client.WithAPIKey(readKey))
		if _, err := c.Stats(ctx); err != nil {
			t.Fatalf("Stats with a read key: %v", err)
		}
		_, err := c.AddRepo(ctx, &client.AddRepoRequest{Name: "x", Source: "local", Path: "/tmp"})
		if !errors.Is(err, client.ErrForbidden) {
			t.Fatalf("AddRepo with a read key gave %v, want ErrForbidden", err)
		}
	})

	t.Run("AdminKeyMayMutate", func(t *testing.T) {
		c := client.New(url, client.WithAPIKey(adminKey))
		if _, err := c.AddRepo(ctx, &client.AddRepoRequest{
			Name: "fixture", Source: "local", Path: servertest.TestdataPath(t, "go-test-project"),
		}); err != nil {
			t.Fatalf("AddRepo with an admin key: %v", err)
		}
	})

	t.Run("BearerIsAcceptedToo", func(t *testing.T) {
		c := client.New(url, client.WithBearerToken(readKey))
		if _, err := c.Stats(ctx); err != nil {
			t.Fatalf("Stats with a bearer token: %v", err)
		}
	})

	t.Run("TheScopePrefixIsNotPartOfTheKey", func(t *testing.T) {
		c := client.New(url, client.WithAPIKey("read:"+readKey))
		if _, err := c.Stats(ctx); !errors.Is(err, client.ErrUnauthorized) {
			t.Fatalf("a key sent with its scope prefix gave %v, want ErrUnauthorized", err)
		}
	})
}

func TestRateLimited(t *testing.T) {
	ctx := context.Background()
	t.Setenv("RAGOTA_BM25_PATH", t.TempDir())

	cfg := servertest.TestConfig(t)
	cfg.Server.RateLimit = &config.RateLimitConfig{Enabled: true, RequestsPerMinute: 1, Burst: 1}
	svc, err := bootstrap.Build(ctx, cfg)
	if err != nil {
		t.Fatalf("setup build: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close(ctx) })
	srv := api.NewServer(svc, &cfg.Server)
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	// Retries off: one request per minute refills slowly enough that a
	// retrying client would sit out the whole test honouring Retry-After.
	c := client.New(ts.URL, client.WithRetries(-1))
	if _, err := c.Stats(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err = c.Stats(ctx)
	if !errors.Is(err, client.ErrRateLimited) {
		t.Fatalf("second call gave %v, want ErrRateLimited", err)
	}
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not a *client.Error: %T", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", apiErr.StatusCode)
	}
	if apiErr.RetryAfter <= 0 {
		t.Errorf("429 carried no backoff hint: %+v", apiErr)
	}
}
