package cli

// Файл содержит хелперы для cli_run: startSSE, waitAndScanVector,
// fanoutWatchEvents, getLSPCommand, getLSPArgs.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"ragota/internal/indexing/ast"
	"ragota/internal/indexing/embedder"
	"ragota/internal/indexing/vector"
	"ragota/pkg/config"
	"ragota/pkg/logger"
	"ragota/pkg/lsp"
	"ragota/pkg/qdrant"
	"ragota/pkg/state"
	"ragota/pkg/watcher"

	"github.com/mark3labs/mcp-go/server"
)

// startSSE поднимает SSE-обёртку над MCPServer на указанном порту.
func startSSE(_ context.Context, wg *sync.WaitGroup, mcp *server.MCPServer, name string, port int) *server.SSEServer {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	sse := server.NewSSEServer(mcp, server.WithBaseURL(baseURL))
	fmt.Fprintf(os.Stderr, "mcp[%s]: serving SSE on %s\n", name, baseURL)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := sse.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "mcp[%s]: SSE server error: %v\n", name, err)
		}
	}()
	return sse
}

// waitAndScanVector ждёт готовности qdrant и ollama, затем full-scan.
func waitAndScanVector(ctx context.Context, qd *qdrant.Client, emb *embedder.Ollama, vIdx *vector.Vector, bus *state.Bus) {
	for range 30 {
		if ctx.Err() != nil {
			return
		}
		pCtx, c2 := context.WithTimeout(ctx, 3*time.Second)
		qErr := qd.Ping(pCtx)
		var oErr error
		if emb != nil {
			oErr = emb.Ping(pCtx)
		}
		c2()
		if qErr == nil && oErr == nil {
			if err := vIdx.Init(ctx); err != nil {
				bus.SetIndexer("vector", func(i *state.Indexer) {
					i.Status = "error"
					i.LastError = "qdrant init: " + err.Error()
				})
				return
			}
			_ = vIdx.FullScan(ctx)
			return
		}
		bus.SetIndexer("vector", func(i *state.Indexer) {
			i.Status = "scanning"
			if qErr != nil {
				i.LastError = "waiting qdrant: " + qErr.Error()
			} else if oErr != nil {
				i.LastError = "waiting ollama: " + oErr.Error()
			}
		})
		time.Sleep(2 * time.Second)
	}
}

// fanoutWatchEvents транслирует события watcher'а во все индексаторы.
func fanoutWatchEvents(ctx context.Context, w *watcher.Watcher, vIdx *vector.Vector, astIdx *astindex.Indexer) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			switch ev.Kind {
			case watcher.EventRemove, watcher.EventRename:
				if vIdx != nil {
					if err := vIdx.RemoveFile(ctx, ev.AbsPath); err != nil {
						logger.Log().Warn().Err(err).Str("path", ev.RelPath).Msg("vector: remove file failed")
					}
				}
				if astIdx != nil {
					if err := astIdx.RemoveFile(ctx, ev.AbsPath); err != nil {
						logger.Log().Warn().Err(err).Str("path", ev.RelPath).Msg("ast: remove file failed")
					}
				}
			default:
				if vIdx != nil {
					if err := vIdx.IndexFile(ctx, ev.AbsPath); err != nil {
						logger.Log().Warn().Err(err).Str("path", ev.RelPath).Msg("vector: index file failed")
					}
				}
				if astIdx != nil {
					if err := astIdx.IndexFile(ctx, ev.AbsPath); err != nil {
						logger.Log().Warn().Err(err).Str("path", ev.RelPath).Msg("ast: index file failed")
					}
				}
			}
		}
	}
}

// getLSPCommand возвращает команду для LSP-сервера по языку.
func getLSPCommand(lang string) string {
	switch lang {
	case "go":
		return "gopls"
	case "typescript", "javascript":
		return "typescript-language-server"
	case "python":
		return "pyright-langserver"
	case "java":
		return "jdtls"
	default:
		return lang + "-language-server"
	}
}

// getLSPArgs возвращает аргументы для LSP-сервера по языку.
func getLSPArgs(lang string) []string {
	switch lang {
	case "typescript", "javascript", "python":
		return []string{"--stdio"}
	case "java":
		return []string{
			"--jvm-arg=-Xmx4G",
			"--jvm-arg=--add-opens=java.base/sun.misc=ALL-UNNAMED",
			"--jvm-arg=--add-opens=java.base/java.lang.reflect=ALL-UNNAMED",
			"--jvm-arg=--add-opens=java.base/java.util=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.api=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.util=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.code=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.main=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.tree=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.model=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.comp=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.file=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.jvm=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.parser=ALL-UNNAMED",
			"--jvm-arg=--add-opens=jdk.compiler/com.sun.tools.javac.processing=ALL-UNNAMED",
			"-data", "/workspace/.ragota/jdtls-data",
		}
	default:
		return nil
	}
}

// cfgToLSPSpecs конвертирует cfg.LSP в []lsp.ServerSpec.
func cfgToLSPSpecs(cfg *config.Config) []lsp.ServerSpec {
	specs := make([]lsp.ServerSpec, 0, len(cfg.LSP))
	for _, s := range cfg.LSP {
		specs = append(specs, lsp.ServerSpec{
			Language:  s.Language,
			Command:   s.Command,
			Args:      s.Args,
			LocalRoot: cfg.Root,
		})
	}
	return specs
}
