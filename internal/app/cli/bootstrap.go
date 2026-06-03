package cli

// Файл содержит общую логику bootstrap для команд run и watch:
// Docker, SQLite, repo discovery, watcher, vector indexer.

import (
	"context"
	"fmt"
	"os"
	"time"

	"ragota/pkg/config"
	"ragota/pkg/docker"
	"ragota/internal/indexing/embedder"
	"ragota/pkg/fileutil"
	"ragota/internal/indexing/vector"
	"ragota/pkg/qdrant"
	"ragota/pkg/repos"
	"ragota/pkg/state"
	"ragota/internal/store"
	"ragota/pkg/watcher"
)

// bootEnv — ресурсы, созданные при bootstrap.
type bootEnv struct {
	st       *store.SQLite
	resolver *repos.Resolver
	runner   *docker.Runner
	repoSig  string
}

// cleanup закрывает store (docker останавливается отдельно через stopDocker).
func (b *bootEnv) cleanup() {
	if b.st != nil {
		_ = b.st.Close()
	}
}

// createVectorIndexer создаёт и запускает vector indexer.
func (b *bootEnv) createVectorIndexer(ctx context.Context, cfg *config.Config, bus *state.Bus) *vector.Vector {
	qd := qdrant.New(fmt.Sprintf("http://%s:%d", cfg.Qdrant.Host, cfg.Qdrant.Port))
	emb := embedder.New(cfg.Ollama.URL, cfg.Ollama.EmbedModel)
	vIdx := vector.NewVector(cfg, qd, emb, b.st, bus)
	vIdx.SetRepoResolver(b.resolver)
	go waitAndScanVector(ctx, qd, emb, vIdx, bus)
	return vIdx
}

// createWatcher создаёт watcher и запускает его.
func (b *bootEnv) createWatcher(ctx context.Context, cfg *config.Config) (*watcher.Watcher, error) {
	matcher := fileutil.NewMatcher(cfg.Ignore)
	w, err := watcher.New(cfg.Root, matcher, cfg.Extensions, 300*time.Millisecond)
	if err != nil {
		return nil, err
	}
	w.SetRepoResolver(b.resolver)
	if err := w.Start(ctx); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

// stopDocker останавливает Docker контейнеры.
func (b *bootEnv) stopDocker(ctx context.Context) {
	if b.runner == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "docker: stopping containers...\n")
	_ = b.runner.Down(ctx)
	fmt.Fprintf(os.Stderr, "docker: containers stopped\n")
}

// bootstrap выполняет общую инициализацию: Docker, store, repo discovery.
func bootstrap(ctx context.Context, cfg *config.Config, bus *state.Bus, envMode string) (*bootEnv, error) {
	startDockerAll := envMode == "docker"
	startDockerQdrant := envMode == "" || envMode == "local" || startDockerAll

	var runner *docker.Runner
	if startDockerQdrant || startDockerAll {
		r, err := bootstrapDocker(ctx, cfg, bus, startDockerAll)
		if err != nil {
			return nil, err
		}
		runner = r
	}
	if startDockerQdrant || startDockerAll {
		cfg.Qdrant.Host = "localhost"
	}

	discovered, err := repos.Discover(cfg.Root)
	if err != nil {
		return nil, err
	}
	if len(discovered) > 1 {
		fmt.Fprintf(os.Stderr, "repos: found %d repos\n", len(discovered))
	}
	sig := repos.Signature(discovered)

	st, err := store.OpenFresh(cfg.SQLitePath(), sig)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}

	return &bootEnv{
		st:       st,
		resolver: repos.NewResolver(discovered),
		runner:   runner,
		repoSig:  sig,
	}, nil
}
