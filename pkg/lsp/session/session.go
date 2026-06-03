// Package session реализует активную LSP-сессию: клиент, подключённый к работающему серверу.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"ragota/pkg/lsp"
	"ragota/pkg/lsp/jsonrpc"
	"ragota/pkg/lsp/lang"
	"ragota/pkg/lsp/process"
)

// Session — активная LSP-сессия. Реализует lsp.Client.
type Session struct {
	Conn *jsonrpc.Conn
	Proc *process.Process
	Lang string

	langCaps *lang.Capabilities

	rootURI    string
	localRoot  string
	hostRoot   string
	remoteRoot string

	mu          sync.Mutex
	openedFiles map[string]string
	openedVers  map[string]int

	diagnosticsReady chan string

	javaReady        chan struct{}
	javaReadyClosed  atomic.Bool
	goplsReady       chan struct{}
	goplsReadyClosed atomic.Bool

	debugLog func(format string, args ...any)
}

// New создаёт новую сессию. Вызывается из lifecycle после запуска процесса и handshake.
func New(
	conn *jsonrpc.Conn,
	proc *process.Process,
	language string,
	langCaps *lang.Capabilities,
	rootURI, localRoot, hostRoot, remoteRoot string,
	debugLog func(format string, args ...any),
) *Session {
	return &Session{
		Conn:             conn,
		Proc:             proc,
		Lang:             language,
		langCaps:         langCaps,
		rootURI:          rootURI,
		localRoot:        localRoot,
		hostRoot:         hostRoot,
		remoteRoot:       remoteRoot,
		openedFiles:      make(map[string]string),
		openedVers:       make(map[string]int),
		diagnosticsReady: make(chan string, 16),
		javaReady:        make(chan struct{}),
		goplsReady:       make(chan struct{}),
		debugLog:         debugLog,
	}
}

func (s *Session) debug(format string, args ...any) {
	if s.debugLog != nil {
		s.debugLog(format, args...)
	}
}

// Language implements lsp.Client.
func (s *Session) Language() string { return s.Lang }

// LangField returns the language string directly (for lifecycle package).
func (s *Session) LangField() string { return s.Lang }

// IsAlive implements lsp.Client.
func (s *Session) IsAlive() bool {
	return s.Proc != nil && s.Proc.IsAlive() && s.Conn != nil && !s.Conn.IsDead()
}

// Close implements lsp.Client.
func (s *Session) Close() error {
	var firstErr error
	if s.Conn != nil {
		if err := s.Conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.Proc != nil {
		s.Proc.Kill()
	}
	return firstErr
}

// EnsureOpen opens a file if not already open. Implements lsp.Client.
func (s *Session) EnsureOpen(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	s.mu.Lock()
	_, exists := s.openedFiles[path]
	s.mu.Unlock()
	if exists {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("lsp: read file for open: %w", err)
	}
	ext := filepath.Ext(path)
	langID := languageIDFromExt(ext, s.Lang)
	return s.DidOpen(ctx, path, langID, string(data))
}

// OnServerNotification is called by jsonrpc.Conn's read loop for server notifications.
func (s *Session) OnServerNotification(method string, params json.RawMessage) {
	switch method {
	case "textDocument/publishDiagnostics":
		var p struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.URI == "" {
			return
		}
		localPath := lsp.ToLocalPath(p.URI, s.remoteRoot, s.hostRoot)
		s.debug("LSP %s: publishDiagnostics: uri=%q local=%q\n", s.Lang, p.URI, localPath)
		select {
		case s.diagnosticsReady <- localPath:
		default:
		}
	case "language/status":
		var p struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		s.debug("LSP %s: language/status: type=%q message=%q\n", s.Lang, p.Type, p.Message)
		switch p.Type {
		case "ServiceReady", "Started", "Ready":
			if s.javaReadyClosed.CompareAndSwap(false, true) {
				close(s.javaReady)
			}
		}
	case "$/progress":
		var p struct {
			Token any `json:"token"`
			Value struct {
				Kind string `json:"kind"`
			} `json:"value"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		tokenStr := fmt.Sprintf("%v", p.Token)
		s.debug("LSP %s: $/progress: token=%q kind=%q\n", s.Lang, tokenStr, p.Value.Kind)
		if (tokenStr == "gopls.indexing" || tokenStr == "Initial workspace scan") && p.Value.Kind == "end" {
			if s.goplsReadyClosed.CompareAndSwap(false, true) {
				close(s.goplsReady)
			}
		}
	}
}

// OnServerRequest is called by jsonrpc.Conn's read loop for server requests.
func (s *Session) OnServerRequest(rawID json.RawMessage, method string, params json.RawMessage) {
	var result any
	switch method {
	case "workspace/configuration":
		n := 1
		var p struct {
			Items []struct {
				ScopeURI string `json:"scopeUri"`
				Section  string `json:"section"`
			} `json:"items"`
		}
		if err := json.Unmarshal(params, &p); err == nil && len(p.Items) > 0 {
			n = len(p.Items)
		}
		arr := make([]any, 0, n)
		for i := 0; i < n; i++ {
			section := ""
			if i < len(p.Items) {
				section = p.Items[i].Section
			}
			arr = append(arr, s.configFor(section))
		}
		result = arr
	case "workspace/workspaceFolders":
		result = []map[string]any{{"uri": s.rootURI, "name": "root"}}
	case "client/registerCapability", "client/unregisterCapability":
		result = nil
	case "window/workDoneProgress/create":
		result = nil
	case "workspace/applyEdit":
		result = map[string]any{"applied": false}
	default:
		resp := jsonrpc.ServerResponse{
			JSONRPC: "2.0",
			ID:      rawID,
			Error:   &jsonrpc.Error{Code: -32601, Message: "method not found: " + method},
		}
		_ = s.Conn.WriteServerResponse(resp)
		return
	}
	resp := jsonrpc.ServerResponse{JSONRPC: "2.0", ID: rawID, Result: result}
	_ = s.Conn.WriteServerResponse(resp)
}

func (s *Session) configFor(section string) any {
	if s.langCaps != nil && s.langCaps.ConfigFor != nil {
		return s.langCaps.ConfigFor(section)
	}
	return map[string]any{}
}

// WaitForJavaReady blocks until jdtls signals readiness or timeout.
func (s *Session) WaitForJavaReady(ctx context.Context, timeout time.Duration) error {
	select {
	case <-s.javaReady:
		return nil
	case <-s.Proc.ProcessDone():
		return fmt.Errorf("jdtls process exited before becoming ready: %v", s.Proc.ProcessErr())
	case <-ctx.Done():
		s.debug("LSP java: caller context cancelled, continuing background init\n")
		return nil
	case <-time.After(timeout):
		s.debug("LSP java: ready timeout after %s, continuing anyway\n", timeout)
		return nil
	}
}

// WaitForGoplsReady blocks until gopls signals initial indexing is done or timeout.
func (s *Session) WaitForGoplsReady(ctx context.Context, timeout time.Duration) error {
	select {
	case <-s.goplsReady:
		return nil
	case <-s.Proc.ProcessDone():
		return fmt.Errorf("gopls process exited before indexing completed: %v", s.Proc.ProcessErr())
	case <-time.After(timeout):
		s.debug("LSP go: indexing wait timeout, continuing\n")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// JavaReady returns the channel that closes when jdtls is ready.
func (s *Session) JavaReady() <-chan struct{} { return s.javaReady }

// GoplsReady returns the channel that closes when gopls indexing is done.
func (s *Session) GoplsReady() <-chan struct{} { return s.goplsReady }

// RootURI returns the root URI of this session.
func (s *Session) RootURI() string { return s.rootURI }

// languageIDFromExt maps file extension to LSP languageId.
func languageIDFromExt(ext, fallback string) string {
	switch ext {
	case ".go":
		return "go"
	case ".java":
		return "java"
	case ".py":
		return "python"
	case ".ts":
		return "typescript"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".tsx":
		return "typescriptreact"
	case ".jsx":
		return "javascriptreact"
	}
	return fallback
}
