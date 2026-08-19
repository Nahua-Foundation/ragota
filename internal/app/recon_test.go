package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/app/enrich"
	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/repos/local"
	"github.com/Nahua-Foundation/ragota/internal/store"
	"github.com/Nahua-Foundation/ragota/internal/store/sqlite"
)

// fakeReconGen is a Generator returning a fixed reply and counting calls.
type fakeReconGen struct {
	mu    sync.Mutex
	calls int
	out   string
}

func (g *fakeReconGen) Name() string { return "fake" }

func (g *fakeReconGen) Generate(context.Context, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.out, nil
}

func (g *fakeReconGen) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// copyDir copies all regular files of src into dst, keeping structure.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// newReconTestService builds a Service over a tmp sqlite store and a local
// repo source (no indexers), suitable for exercising doIndex directly.
func newReconTestService(t *testing.T) (*Service, *sqlite.SQLite) {
	t.Helper()
	st, err := sqlite.Open(&sqlite.Config{Path: filepath.Join(t.TempDir(), "meta.db"), PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	src := local.New()
	if err := src.Init(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	svc := New(&config.Config{}, st, nil,
		map[domain.SourceType]repos.RepoSource{domain.SourceTypeLocal: src}, nil)
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	return svc, st
}

const fakeReconJSON = `{"services":[{"name":"payments","root":"pay","purpose":"payment processing","language":"typescript"}],` +
	`"config_paths":["package.json"],"notes":"one extra service under pay/"}`

func TestReconPass(t *testing.T) {
	ctx := context.Background()

	// Temporary copy of testdata/web-ts with an extra pay/ directory that has
	// no build manifest, so heuristic detection cannot see it as a app.
	dir := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "testdata", "web-ts"), dir)
	if err := os.MkdirAll(filepath.Join(dir, "pay"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pay", "charge.ts"),
		[]byte("export function charge(amount: number): number { return amount; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc, st := newReconTestService(t)
	gen := &fakeReconGen{out: "```json\n" + fakeReconJSON + "\n```"}
	svc.SetReconAssistant(gen, true, false)

	repo := &domain.Repo{ID: "web-1", Name: "web", Source: domain.SourceTypeLocal, Path: dir}
	if err := svc.doIndex(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	if got := gen.callCount(); got != 1 {
		t.Fatalf("generator calls after first index = %d, want 1", got)
	}

	// The recon unit stores the raw JSON answer (fences stripped).
	recons, err := st.GetASTUnits(ctx, domain.QueryOpts{RepoID: "web-1", Kind: enrich.KindRecon})
	if err != nil {
		t.Fatal(err)
	}
	if len(recons) != 1 {
		t.Fatalf("recon units = %d, want 1", len(recons))
	}
	ru := recons[0]
	if ru.Qualified != "recon:web-1" || ru.FilePath != enrich.ReconFilePath {
		t.Errorf("recon unit key = %q %q, want recon:web-1 %s", ru.Qualified, ru.FilePath, enrich.ReconFilePath)
	}
	if ru.Doc != fakeReconJSON {
		t.Errorf("recon unit Doc = %q, want the raw JSON without fences", ru.Doc)
	}
	if meta := store.DecodeUnitMeta(ru.Meta); meta.DetectedBy != "llm" {
		t.Errorf("recon unit DetectedBy = %q, want llm", meta.DetectedBy)
	}

	// The LLM hint became a service unit alongside the detected root app.
	assertServices := func() {
		t.Helper()
		units, err := st.GetASTUnits(ctx, domain.QueryOpts{RepoID: "web-1", Kind: store.KindService})
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]*domain.ASTUnit{}
		for _, u := range units {
			byName[u.Name] = u
		}
		pay, ok := byName["payments"]
		if !ok {
			t.Fatalf("service payments not found; got %v", names(units))
		}
		payMeta := store.DecodeUnitMeta(pay.Meta)
		if payMeta.DetectedBy != "llm" || payMeta.Root != "pay" {
			t.Errorf("payments meta = %+v, want DetectedBy llm, Root pay", payMeta)
		}
		web, ok := byName["web"]
		if !ok {
			t.Fatalf("detected root service web not found; got %v", names(units))
		}
		if meta := store.DecodeUnitMeta(web.Meta); meta.DetectedBy == "llm" {
			t.Errorf("web service DetectedBy = llm, want a heuristic detector")
		}
	}
	assertServices()

	// A second index run reuses the stored recon unit: no new LLM call, the
	// same services are re-created.
	if err := svc.doIndex(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	if got := gen.callCount(); got != 1 {
		t.Errorf("generator calls after second index = %d, want still 1", got)
	}
	assertServices()
}

func names(units []*domain.ASTUnit) []string {
	var out []string
	for _, u := range units {
		out = append(out, u.Name)
	}
	return out
}

func TestReconFailureDoesNotFailIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "testdata", "web-ts"), dir)

	svc, st := newReconTestService(t)
	// Unparsable answer: recon must be skipped with a warning, not fail doIndex.
	gen := &fakeReconGen{out: "sorry, I cannot help with that"}
	svc.SetReconAssistant(gen, true, false)

	repo := &domain.Repo{ID: "web-2", Name: "web", Source: domain.SourceTypeLocal, Path: dir}
	if err := svc.doIndex(ctx, repo, false); err != nil {
		t.Fatalf("doIndex must not fail on recon errors, got %v", err)
	}
	recons, err := st.GetASTUnits(ctx, domain.QueryOpts{RepoID: "web-2", Kind: enrich.KindRecon})
	if err != nil {
		t.Fatal(err)
	}
	if len(recons) != 0 {
		t.Errorf("recon units after failed recon = %d, want 0", len(recons))
	}
	units, err := st.GetASTUnits(ctx, domain.QueryOpts{RepoID: "web-2", Kind: store.KindService})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || units[0].Name != "web" {
		t.Errorf("services = %v, want the single detected root service", names(units))
	}
}
