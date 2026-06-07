package cli

// Файл содержит общую логику bootstrap для команды up:
// Docker, SQLite, repo discovery, watcher, vector indexer.

import (
	"context"
	"fmt"
	"os"
	"time"

	"ragota/internal/indexing/embedder"
	"ragota/internal/indexing/vector"
	"ragota/internal/search/bm25"
	"ragota/internal/store"
	"ragota/pkg/config"
	"ragota/pkg/docker"
	"ragota/pkg/fileutil"
	"ragota/pkg/qdrant"
	"ragota/pkg/repos"
	"ragota/pkg/state"
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

// createVectorIndexer создаёт vector indexer и запускает wait+scan.
// bm25Idx передаётся как параметр, чтобы SetBM25 вызвался ДО запуска FullScan
// (избегаем race condition: FullScan идёт асинхронно).
func (b *bootEnv) createVectorIndexer(ctx context.Context, cfg *config.Config, bus *state.Bus, bm25Idx bm25.Index) *vector.Vector {
	qd := qdrant.New(b.runner.QdrantURL())
	emb := embedder.New(cfg.Ollama.URL, cfg.Ollama.EmbedModel)
	vIdx := vector.NewVector(cfg, qd, emb, b.st, bus)
	vIdx.SetRepoResolver(b.resolver)
	// Подключаем BM25 СИНХРОННО, до запуска FullScan goroutine
	if bm25Idx != nil {
		vIdx.SetBM25(bm25.AsWriteSink(bm25Idx))
	}
	go waitAndScanVector(ctx, qd, emb, vIdx, bus)
	return vIdx
}

// createWatcher создаёт и запускает watcher.
func (b *bootEnv) createWatcher(ctx context.Context, cfg *config.Config) (*watcher.Watcher, error) {
	matcher := fileutil.NewMatcher(cfg.IgnorePatterns)
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
}

// bootstrap выполняет инициализацию: Docker, store, repo discovery.
// Docker запускается всегда.
func bootstrap(ctx context.Context, cfg *config.Config, bus *state.Bus) (*bootEnv, error) {
	runner := docker.New(cfg.Root)

	if err := docker.Available(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "docker: check failed: %v\n", err)
		bus.SetDocker(state.DockerStatus{LastError: err.Error()})
		return nil, fmt.Errorf("docker required but unavailable: %w", err)
	}

	fmt.Fprintf(os.Stderr, "docker: starting containers...\n")
	bus.SetDocker(state.DockerStatus{LastError: "starting..."})
	if err := runner.Up(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "docker: error: %v\n", err)
		bus.SetDocker(state.DockerStatus{LastError: err.Error()})
		return nil, fmt.Errorf("docker containers failed to start: %w", err)
	}

	fmt.Fprintf(os.Stderr, "docker: waiting for Qdrant to become ready...\n")
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := runner.WaitQdrant(waitCtx); err != nil {
		fmt.Fprintf(os.Stderr, "docker: Qdrant not ready: %v (continuing anyway)\n", err)
	}
	waitCancel()
	fmt.Fprintf(os.Stderr, "docker: all containers are up\n")

	// Сохраняем URL для остального кода
	cfg.QdrantURL = runner.QdrantURL()

	go startDockerMonitor(ctx, runner, bus)

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
