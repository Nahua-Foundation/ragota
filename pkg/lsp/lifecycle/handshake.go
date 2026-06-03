package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"ragota/pkg/lsp"
	"ragota/pkg/lsp/lang"
	"ragota/pkg/lsp/session"
)

// Initialize performs the LSP handshake: initialize + initialized.
// For Java, waits for jdtls ready signal. For Go, waits for gopls indexing.
func Initialize(ctx context.Context, sess *session.Session, root string) error {
	conn := sess.Conn
	language := sess.Language()
	rootURI := sess.RootURI()

	langReg := lang.Default()
	var langCaps *lang.Capabilities
	if langReg != nil {
		langCaps = langReg.Get(language)
	}
	if langCaps == nil {
		defaultCaps := lang.DefaultCapabilities()
		langCaps = &lang.Capabilities{
			ClientCapabilities: func() map[string]any { return defaultCaps },
		}
	}

	params := map[string]any{
		"processId":    nil,
		"rootPath":     lsp.PathFromFileURI(rootURI),
		"rootUri":      rootURI,
		"capabilities": langCaps.ClientCapabilities(),
		"workspaceFolders": []map[string]any{
			{"uri": rootURI, "name": filepath.Base(lsp.PathFromFileURI(rootURI))},
		},
	}
	if langCaps.InitOptions != nil {
		if initOpts := langCaps.InitOptions(); initOpts != nil {
			params["initializationOptions"] = initOpts
		}
	}

	var res json.RawMessage
	if err := conn.Call(ctx, "initialize", params, &res); err != nil {
		return err
	}
	if err := conn.Notify("initialized", map[string]any{}); err != nil {
		return err
	}

	// Send workspace/didChangeConfiguration
	if langCaps.ConfigFor != nil {
		if settings := langCaps.ConfigFor(""); settings != nil {
			_ = conn.Notify("workspace/didChangeConfiguration", map[string]any{
				"settings": settings,
			})
		}
	}
	if language == "java" {
		_ = conn.Notify("workspace/didChangeConfiguration", map[string]any{
			"settings": map[string]any{
				"java": lang.JavaConfigFor("java"),
			},
		})
		readyTimeout := 120 * time.Second
		if v := os.Getenv("JDTLS_READY_TIMEOUT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				readyTimeout = time.Duration(n) * time.Second
			}
		}
		if err := sess.WaitForJavaReady(ctx, readyTimeout); err != nil {
			return fmt.Errorf("jdtls ready: %w", err)
		}
	}

	if language == "go" {
		if err := sess.WaitForGoplsReady(ctx, 30*time.Second); err != nil {
			return err
		}
	}

	return nil
}
