package session

import (
	"context"
	"encoding/json"
	"time"

	"ragota/pkg/lsp"
)

// Definition implements lsp.Client.
func (s *Session) Definition(ctx context.Context, path string, line, character int) ([]lsp.Location, error) {
	params := s.positionParams(path, line, character)
	var raw json.RawMessage
	if err := s.Conn.Call(ctx, "textDocument/definition", params, &raw); err != nil {
		s.debug("LSP %s: Definition ERROR: %v\n", s.Lang, err)
		return nil, err
	}
	locs := lsp.DecodeLocations(raw, s.remoteRoot, s.hostRoot)
	s.debug("LSP %s: Definition RESULT: locations=%d raw=%q\n", s.Lang, len(locs), string(raw))
	if len(locs) > 0 {
		return locs, nil
	}

	// fallback: declaration, typeDefinition
	for _, method := range []string{"textDocument/declaration", "textDocument/typeDefinition"} {
		fallbackCtx, cancel := context.WithCancel(ctx)
		var r json.RawMessage
		if err := s.Conn.Call(fallbackCtx, method, params, &r); err != nil {
			cancel()
			continue
		}
		cancel()
		if locs := lsp.DecodeLocations(r, s.remoteRoot, s.hostRoot); len(locs) > 0 {
			s.debug("LSP %s: %s RESULT: locations=%d\n", s.Lang, method, len(locs))
			return locs, nil
		}
	}

	// Heuristic: retry at shifted position (find start of identifier)
	lineText := localFileLine(path, line)
	if lineText != "" && character > 0 {
		newChar := character
		if character >= len(lineText) || !isAlphaNum(lineText[character]) {
			newChar--
		}
		for newChar > 0 && isAlphaNum(lineText[newChar]) {
			newChar--
		}
		if newChar < character {
			if !isAlphaNum(lineText[newChar]) {
				newChar++
			}
			if newChar != character {
				s.debug("LSP %s: Definition retry at shifted position: %d -> %d\n", s.Lang, character, newChar)
				return s.Definition(ctx, path, line, newChar)
			}
		}
	}

	return nil, nil
}

// References implements lsp.Client.
func (s *Session) References(ctx context.Context, path string, line, character int, includeDecl bool) ([]lsp.Location, error) {
	params := s.positionParams(path, line, character)
	params["context"] = map[string]any{"includeDeclaration": includeDecl}
	var raw json.RawMessage
	if err := s.Conn.Call(ctx, "textDocument/references", params, &raw); err != nil {
		s.debug("LSP %s: References ERROR: %v\n", s.Lang, err)
		return nil, err
	}
	locs := lsp.DecodeLocations(raw, s.remoteRoot, s.hostRoot)
	s.debug("LSP %s: References RESULT: locations=%d raw=%q\n", s.Lang, len(locs), string(raw))
	return locs, nil
}

// Hover implements lsp.Client.
func (s *Session) Hover(ctx context.Context, path string, line, character int) (string, error) {
	params := s.positionParams(path, line, character)
	var raw json.RawMessage
	start := time.Now()
	if err := s.Conn.Call(ctx, "textDocument/hover", params, &raw); err != nil {
		s.debug("LSP %s: Hover ERROR: %v (elapsed %v)\n", s.Lang, err, time.Since(start))
		return "", err
	}
	if len(raw) == 0 || string(raw) == "null" {
		s.debug("LSP %s: Hover RESULT: empty (elapsed %v)\n", s.Lang, time.Since(start))
		return "", nil
	}
	var h struct {
		Contents any `json:"contents"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		s.debug("LSP %s: Hover ERROR parse: %v (elapsed %v)\n", s.Lang, err, time.Since(start))
		return "", err
	}
	txt := lsp.HoverString(h.Contents)
	s.debug("LSP %s: Hover RESULT: %d chars (elapsed %v)\n", s.Lang, len(txt), time.Since(start))
	return txt, nil
}

// Implementation implements lsp.Client.
func (s *Session) Implementation(ctx context.Context, path string, line, character int) ([]lsp.Location, error) {
	if s.Lang == "python" {
		s.debug("LSP %s: Implementation NOT SUPPORTED at protocol level\n", s.Lang)
		return nil, nil
	}
	params := s.positionParams(path, line, character)
	var raw json.RawMessage
	if err := s.Conn.Call(ctx, "textDocument/implementation", params, &raw); err != nil {
		s.debug("LSP %s: Implementation ERROR: %v raw=%q\n", s.Lang, err, string(raw))
		return nil, err
	}
	locs := lsp.DecodeLocations(raw, s.remoteRoot, s.hostRoot)
	s.debug("LSP %s: Implementation RESULT: locations=%d raw=%q\n", s.Lang, len(locs), string(raw))
	if len(locs) == 0 && s.Lang == "java" {
		s.debug("LSP %s: Implementation empty, trying references as fallback\n", s.Lang)
		return s.References(ctx, path, line, character, false)
	}
	return locs, nil
}

// positionParams builds the textDocument/position parameter map.
func (s *Session) positionParams(path string, line, character int) map[string]any {
	remotePath := lsp.ToRemotePath(path, s.hostRoot, s.remoteRoot)
	remoteURI := lsp.FileURI(remotePath)
	return map[string]any{
		"textDocument": map[string]any{"uri": remoteURI},
		"position":     map[string]any{"line": line, "character": character},
	}
}
