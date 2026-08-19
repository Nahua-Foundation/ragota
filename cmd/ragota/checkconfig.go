package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/llm"
	"github.com/Nahua-Foundation/ragota/pkg/httpx"
)

// probeTimeout bounds one reachability check. It is short on purpose:
// --check-config is meant to run in a deploy pipeline.
const probeTimeout = 3 * time.Second

// probe is one dependency the running server would talk to.
type probe struct {
	name   string // config path, e.g. "search.rerank.base_url"
	target string // host:port or URL
	err    error
}

// runCheckConfig validates the configuration and probes every configured
// endpoint without opening the database or creating any index. It returns the
// process exit code.
func runCheckConfig(cfg *config.Config, path string) int {
	fmt.Printf("config: %s\n", path)

	// Catch typo'd/unknown keys, which the lenient Load silently ignores.
	if err := config.CheckUnknownKeys(path); err != nil {
		fmt.Println("\nINVALID")
		fmt.Printf("  %s\n", err)
		return exitConfigInvalid
	}

	if err := cfg.Validate(); err != nil {
		fmt.Println("\nINVALID")
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Printf("  %s\n", line)
		}
		return exitConfigInvalid
	}
	fmt.Println("validation: OK")

	for _, w := range cfg.Warnings() {
		fmt.Printf("warning: %s\n", w)
	}

	probes := dependencyProbes(cfg)
	if len(probes) == 0 {
		fmt.Println("wiring: nothing to probe (no external dependency configured)")
		return 0
	}

	fmt.Println("\nwiring dry-run (no database access):")
	failed := 0
	for _, p := range probes {
		if p.err != nil {
			failed++
			fmt.Printf("  FAIL  %-34s %-32s %v\n", p.name, p.target, p.err)
			continue
		}
		fmt.Printf("  OK    %-34s %s\n", p.name, p.target)
	}

	if failed > 0 {
		fmt.Printf("\n%d of %d dependencies unreachable\n", failed, len(probes))
		return exitDepUnreachable
	}
	fmt.Printf("\nall %d dependencies reachable\n", len(probes))
	return 0
}

// dependencyProbes builds and runs the probe list. The database is
// deliberately absent: --check-config must be safe to run against production
// config from a laptop.
func dependencyProbes(cfg *config.Config) []probe {
	var probes []probe

	add := func(name, target string) {
		if target == "" {
			return
		}
		probes = append(probes, probe{name: name, target: target, err: httpx.DialTarget(target, probeTimeout)})
	}

	if cfg.Storage.Qdrant != nil {
		add("store.qdrant.url", cfg.Storage.Qdrant.URL)
	}
	if cfg.Search != nil && cfg.Search.Rerank != nil && cfg.Search.Rerank.Enabled {
		add("search.rerank.base_url", cfg.Search.Rerank.BaseURL)
	}
	if cfg.Indexes.Vector != nil && cfg.Indexes.Vector.Enabled {
		provider := cfg.Indexes.Vector.Embedder.Provider
		endpoint := cfg.Indexes.Vector.Embedder.BaseURL
		if endpoint == "" {
			endpoint = cfg.Models.Providers[provider].BaseURL
		}
		if endpoint == "" {
			// The same fallback the embedder constructors use: the probe must
			// dial what the server will dial.
			endpoint = llm.DefaultBaseURL(provider)
		}
		add("indexes.vector.embedder ("+provider+")", endpoint)
	}
	if cfg.Summaries != nil && cfg.Summaries.Enabled {
		provider := cfg.Summaries.Provider
		if provider == "" {
			provider = "ollama"
		}
		add("summaries ("+provider+")", cfg.Models.Providers[provider].BaseURL)
	}
	if a := cfg.Models.Assistant; a != nil {
		add("models.assistant.base_url", a.BaseURL)
	}
	if cfg.LSP != nil && cfg.LSP.Enabled {
		langs := make([]string, 0, len(cfg.LSP.Servers))
		for lang := range cfg.LSP.Servers {
			langs = append(langs, lang)
		}
		sort.Strings(langs)
		for _, lang := range langs {
			add("lsp.servers."+lang, cfg.LSP.Servers[lang].Addr)
		}
	}

	return probes
}
