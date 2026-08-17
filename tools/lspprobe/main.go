// Command lspprobe asks a language server what it can actually answer for one
// repository, and what that answer costs.
//
// The LSP passes are only as good as the session behind them, and a server
// that cannot load a workspace fails the same way as one with nothing to say:
// it answers, quickly, with nothing. This makes the difference visible before
// a whole indexing run is spent on it — it prints the workspace-load time, the
// symbols documentSymbol returns per file, and the time and result count of
// each textDocument/references request.
//
//	lspprobe -addr 127.0.0.1:7303 -lang java \
//	    -host-root /data/corpus -repo /data/corpus/petclinic \
//	    -files spring-petclinic-api-gateway/src/.../CustomersServiceClient.java
//
//	initialize: 8.1s
//	…/CustomersServiceClient.java: didOpen+documentSymbol 3m14s, 5 symbols
//	  getOwner    3 refs in 307ms
//	        …/ApiGatewayController.java:56
//
// Zero symbols means the workspace did not load (for jdtls and OmniSharp,
// usually a read-only mount: both write build metadata next to the project).
// Zero references for a symbol that visibly has callers means the same thing
// one level down — which is why lsp.CallRefiner treats an empty answer as "no
// information" rather than as "no callers".
//
// -settle waits after didOpen for servers that import a project in the
// background; -repeat re-asks each symbol, which separates the cost of the
// first request (workspace load) from the steady-state cost.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/lsp"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7311", "language server address")
	hostRoot := flag.String("host-root", "", "host directory mounted in the container")
	mountRoot := flag.String("mount-root", "/workspace", "where host-root is mounted")
	repo := flag.String("repo", "", "repository path on the host")
	lang := flag.String("lang", "go", "ragota language name")
	files := flag.String("files", "", "comma-separated repo-relative files to probe")
	maxSyms := flag.Int("max-symbols", 10, "references requests per file")
	timeout := flag.Duration("timeout", 180*time.Second, "per-request timeout")
	tsInit := flag.Bool("ts-init", false, "pass the typescript-language-server tsserver path")
	settle := flag.Duration("settle", 0, "wait after didOpen before asking for references")
	repeat := flag.Int("repeat", 1, "ask references this many times per symbol")
	flag.Parse()

	mapper := lsp.NewMapper(*hostRoot, *mountRoot)
	client, err := lsp.Dial(*addr, *timeout)
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer client.Close()

	ctx := context.Background()
	var initOpts map[string]any
	if *tsInit {
		initOpts = map[string]any{"tsserver": map[string]any{"path": "/usr/local/lib/node_modules/typescript/lib"}}
	}
	t0 := time.Now()
	if err := client.Initialize(ctx, mapper.ToURI(*repo), initOpts); err != nil {
		fmt.Println("initialize:", err)
		os.Exit(1)
	}
	fmt.Printf("initialize: %s\n", time.Since(t0).Round(time.Millisecond))

	for _, rel := range strings.Split(*files, ",") {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		abs := filepath.Join(*repo, rel)
		content, err := os.ReadFile(abs)
		if err != nil {
			fmt.Println("read:", err)
			continue
		}
		uri := mapper.ToURI(abs)
		t := time.Now()
		if err := client.DidOpen(uri, lsp.LanguageID(*lang), string(content)); err != nil {
			fmt.Println("didOpen:", err)
			continue
		}
		syms, err := client.DocumentSymbols(ctx, uri)
		if err != nil {
			fmt.Printf("%s documentSymbol: %v\n", rel, err)
			continue
		}
		fmt.Printf("\n%s: didOpen+documentSymbol %s, %d symbols\n",
			rel, time.Since(t).Round(time.Millisecond), len(syms))

		if *settle > 0 {
			time.Sleep(*settle)
		}
		sort.Slice(syms, func(i, j int) bool { return syms[i].SelLine < syms[j].SelLine })
		n, total := 0, time.Duration(0)
		for round := 0; round < *repeat; round++ {
			for _, s := range syms {
				if n >= *maxSyms**repeat {
					break
				}
				if s.Kind != 6 && s.Kind != 12 && s.Kind != 9 { // method, function, constructor
					continue
				}
				t := time.Now()
				locs, err := client.References(ctx, uri, s.SelLine, s.SelChar)
				d := time.Since(t)
				total += d
				n++
				if err != nil {
					fmt.Printf("  %-40s references: %v (%s)\n", s.Name, err, d.Round(time.Millisecond))
					continue
				}
				fmt.Printf("  %-40s %3d refs in %s\n", s.Name, len(locs), d.Round(time.Millisecond))
				for i, l := range locs {
					if i >= 3 {
						break
					}
					fmt.Printf("        %s:%d\n", mapper.FromURI(l.URI), l.Line+1)
				}
			}
		}
		if n > 0 {
			fmt.Printf("  -> %d references requests, %s total, %s mean\n",
				n, total.Round(time.Millisecond), (total / time.Duration(n)).Round(time.Millisecond))
		}
	}
}
