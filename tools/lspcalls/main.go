// Command lspcalls runs the language-server call-edge pass (lsp.CallRefiner)
// over an index that already exists.
//
// Two uses. First, measurement: the pass only rewrites edges, so running it
// between two evaluation runs makes the retrieval channels byte-identical on
// both sides and isolates the graph change — one index instead of two, and no
// embedding cost for the B side. Second, operations: the pass is the expensive
// part of indexing with language servers, and this runs it (or re-runs it for
// one repository) without a reindex.
//
//	lspcalls -db ~/.ragota-core/data/ragota.db -corpus /data/corpus \
//	    -host-root /data/corpus -repos petclinic -langs java -scope both
//
//	== petclinic
//	   [candidates 60 asked 59 files 21 references 10 confirmed 1 repointed 5
//	    added 4 contradicted 0 truncated false languages java failed  seconds 30]
//
// -out writes the same counters per repository as JSON. The server must not be
// running against the same SQLite file.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/lsp"
	"github.com/Nahua-Foundation/ragota/internal/storage/sqlite"
)

func main() {
	dbPath := flag.String("db", "", "sqlite index to correct")
	corpus := flag.String("corpus", "", "corpus root (host paths of the repositories)")
	hostRoot := flag.String("host-root", "", "lsp.host_root")
	mountRoot := flag.String("mount-root", "/workspace", "lsp.mount_root")
	only := flag.String("repos", "", "comma-separated repo names (default: all)")
	scope := flag.String("scope", "both", "lsp.calls.scope")
	maxSymbols := flag.Int("max-symbols", 4000, "lsp.calls.max_symbols")
	maxRefs := flag.Int("max-refs", 200, "lsp.calls.max_refs_per_symbol")
	timeout := flag.Int("timeout", 300, "lsp.timeout_seconds")
	langs := flag.String("langs", "go,java,csharp,typescript", "languages to enable")
	addrs := flag.String("addrs", "go=127.0.0.1:7311,typescript=127.0.0.1:7312,java=127.0.0.1:7313,csharp=127.0.0.1:7314", "lang=addr list")
	out := flag.String("out", "", "write the per-repo cost/effect table here as JSON")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	servers := map[string]config.LSPServerConfig{}
	byLang := map[string]string{}
	for _, pair := range strings.Split(*addrs, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok {
			byLang[k] = v
		}
	}
	for _, lang := range strings.Split(*langs, ",") {
		lang = strings.TrimSpace(lang)
		addr := byLang[lang]
		if addr == "" {
			continue
		}
		srv := config.LSPServerConfig{Addr: addr}
		if lang == "typescript" {
			srv.InitOptions = map[string]any{
				"tsserver": map[string]any{"path": "/usr/local/lib/node_modules/typescript/lib"},
			}
		}
		servers[lang] = srv
	}

	st, err := sqlite.Open(&sqlite.Config{Path: *dbPath, PoolSize: 4})
	if err != nil {
		fmt.Println("open sqlite:", err)
		os.Exit(1)
	}
	ctx := context.Background()
	if err := st.Init(ctx); err != nil {
		fmt.Println("init sqlite:", err)
		os.Exit(1)
	}
	defer st.Close()

	refiner := lsp.NewCallRefiner(st, &config.LSPConfig{
		Enabled:        true,
		HostRoot:       *hostRoot,
		MountRoot:      *mountRoot,
		TimeoutSeconds: *timeout,
		Servers:        servers,
		Calls: &config.LSPCallsConfig{
			Enabled:          true,
			Scope:            *scope,
			MaxSymbols:       *maxSymbols,
			MaxRefsPerSymbol: *maxRefs,
		},
	})
	if refiner == nil {
		fmt.Println("call refiner disabled")
		os.Exit(1)
	}

	repos, err := st.ListRepos(ctx)
	if err != nil {
		fmt.Println("list repos:", err)
		os.Exit(1)
	}
	wanted := map[string]bool{}
	for _, n := range strings.Split(*only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			wanted[n] = true
		}
	}

	type row struct {
		Repo         string             `json:"repo"`
		Candidates   int                `json:"candidates"`
		Asked        int                `json:"asked"`
		Requests     int                `json:"requests"`
		Files        int                `json:"files"`
		References   int                `json:"references"`
		Confirmed    int                `json:"confirmed"`
		Repointed    int                `json:"repointed"`
		Added        int                `json:"added"`
		Contradicted int                `json:"contradicted"`
		Truncated    bool               `json:"truncated"`
		Languages    []string           `json:"languages"`
		Failed       []string           `json:"failed"`
		Seconds      float64            `json:"seconds"`
		LangSeconds  map[string]float64 `json:"lang_seconds"`
	}
	var rows []row
	for _, r := range repos {
		if len(wanted) > 0 && !wanted[r.Name] {
			continue
		}
		path := r.Path
		if *corpus != "" {
			path = *corpus + "/" + r.Name
		}
		fmt.Printf("== %s (%s)\n", r.Name, path)
		t0 := time.Now()
		stats, err := refiner.RefineRepo(ctx, r.ID, path)
		if err != nil {
			fmt.Printf("   error: %v\n", err)
		}
		if stats == nil {
			continue
		}
		rows = append(rows, row{
			Repo: r.Name, Candidates: stats.Candidates, Asked: stats.Asked,
			Requests: stats.Requests, Files: stats.FilesOpened, References: stats.References,
			Confirmed: stats.Confirmed, Repointed: stats.Repointed, Added: stats.Added,
			Contradicted: stats.Contradicted, Truncated: stats.Truncated,
			Languages: stats.Languages, Failed: stats.Failed,
			Seconds: time.Since(t0).Seconds(), LangSeconds: stats.LangSeconds,
		})
		fmt.Printf("   %v\n", stats.Log())
	}
	if *out != "" {
		b, _ := json.MarshalIndent(rows, "", "  ")
		_ = os.WriteFile(*out, b, 0o644)
		fmt.Println("wrote", *out)
	}
}
