package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ragota/pkg/lsp"
)

// DidOpen implements lsp.Client. Sends textDocument/didOpen or didChange.
func (s *Session) DidOpen(ctx context.Context, path, languageID, text string) error {
	s.mu.Lock()
	old, exists := s.openedFiles[path]
	if exists && old == text {
		s.mu.Unlock()
		return nil
	}
	s.openedFiles[path] = text
	s.openedVers[path]++
	ver := s.openedVers[path]
	s.mu.Unlock()

	remotePath := lsp.ToRemotePath(path, s.hostRoot, s.remoteRoot)
	remoteURI := lsp.FileURI(remotePath)

	s.debug("LSP %s: DidOpen: local=%q remote=%q uri=%q rootURI=%q ver=%d\n",
		s.Lang, path, remotePath, remoteURI, s.rootURI, ver)

	method := "textDocument/didOpen"
	params := map[string]any{
		"textDocument": map[string]any{
			"uri":        remoteURI,
			"languageId": languageID,
			"version":    ver,
			"text":       text,
		},
	}
	if exists {
		method = "textDocument/didChange"
		params = map[string]any{
			"textDocument": map[string]any{
				"uri":     remoteURI,
				"version": ver,
			},
			"contentChanges": []any{
				map[string]any{"text": text},
			},
		}
	}

	if err := s.Conn.Notify(method, params); err != nil {
		s.mu.Lock()
		delete(s.openedFiles, path)
		delete(s.openedVers, path)
		s.mu.Unlock()
		return err
	}
	if method == "textDocument/didOpen" {
		_ = s.Conn.Notify("workspace/didChangeWatchedFiles", map[string]any{
			"changes": []any{
				map[string]any{"uri": remoteURI, "type": 1},
			},
		})
	}
	if method == "textDocument/didOpen" {
		time.Sleep(100 * time.Millisecond)
		timeout := 3 * time.Second
		switch s.Lang {
		case "java":
			timeout = 15 * time.Second
		case "go":
			timeout = 5 * time.Second
		}
		if !s.waitForDiagnosticsCtx(ctx, path, timeout) {
			s.mu.Lock()
			delete(s.openedFiles, path)
			delete(s.openedVers, path)
			s.mu.Unlock()
			return ctx.Err()
		}
	}
	return nil
}

func (s *Session) waitForDiagnostics(path string, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	started := time.Now()
	for {
		select {
		case uriPath := <-s.diagnosticsReady:
			if lsp.SamePath(uriPath, path) {
				s.debug("LSP %s: waitForDiagnostics DONE: path=%q elapsed=%v\n", s.Lang, path, time.Since(started))
				return
			}
		case <-deadline.C:
			s.debug("LSP %s: waitForDiagnostics TIMEOUT: path=%q after %v\n", s.Lang, path, time.Since(started))
			return
		}
	}
}

func (s *Session) waitForDiagnosticsCtx(ctx context.Context, path string, timeout time.Duration) bool {
	if timeout <= 0 {
		return true
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	started := time.Now()
	for {
		select {
		case <-ctx.Done():
			s.debug("LSP %s: waitForDiagnosticsCtx CANCELLED: path=%q\n", s.Lang, path)
			return false
		case uriPath := <-s.diagnosticsReady:
			if lsp.SamePath(uriPath, path) {
				s.debug("LSP %s: waitForDiagnosticsCtx DONE: path=%q elapsed=%v\n", s.Lang, path, time.Since(started))
				return true
			}
		case <-deadline.C:
			s.debug("LSP %s: waitForDiagnosticsCtx TIMEOUT: path=%q after %v\n", s.Lang, path, time.Since(started))
			return true
		}
	}
}

// localFileLine reads a specific line from a local file (best-effort).
func localFileLine(path string, line int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if line < 0 || line >= len(lines) {
		return ""
	}
	return lines[line]
}

// isAlphaNum returns true for ASCII alphanumeric and underscore.
func isAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}
