package mcp

// Файл содержит LSP-хендлеры: handleDefinition, handleReferences,
// handleHover, handleImplementation.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"ragota/pkg/lsp"

	"github.com/mark3labs/mcp-go/mcp"
)

func (s *LSPServer) handleDefinition(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	if file == "" {
		return mcp.NewToolResultError("file is required"), nil
	}
	abs, err := s.resolveAbs(file)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	line := req.GetInt("line", 0)
	char := req.GetInt("character", 0)

	lspCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var lspErr error
	var locs []lsp.Location
	c, err := s.mgr.EnsureOpen(lspCtx, abs)
	if err == nil {
		locs, lspErr = c.Definition(lspCtx, abs, line, char)
	} else {
		lspErr = err
	}

	if lspErr != nil {
		return s.fallbackToTreeSitter(ctx, "definition", abs, line, char, lspErr)
	}
	return jsonResult(locs)
}

func (s *LSPServer) handleReferences(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	if file == "" {
		return mcp.NewToolResultError("file is required"), nil
	}
	abs, err := s.resolveAbs(file)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	line := req.GetInt("line", 0)
	char := req.GetInt("character", 0)
	includeDecl := req.GetBool("include_declaration", true)

	lspCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var lspErr error
	var locs []lsp.Location
	c, err := s.mgr.EnsureOpen(lspCtx, abs)
	if err == nil {
		locs, lspErr = c.References(lspCtx, abs, line, char, includeDecl)
	} else {
		lspErr = err
	}

	if lspErr != nil {
		return s.fallbackToTreeSitter(ctx, "references", abs, line, char, lspErr)
	}
	return jsonResult(locs)
}

func (s *LSPServer) handleHover(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	if file == "" {
		return mcp.NewToolResultError("file is required"), nil
	}
	abs, err := s.resolveAbs(file)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	line := req.GetInt("line", 0)
	char := req.GetInt("character", 0)

	lspCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var lspErr error
	var txt string
	c, err := s.mgr.EnsureOpen(lspCtx, abs)
	if err == nil {
		txt, lspErr = c.Hover(lspCtx, abs, line, char)
	} else {
		lspErr = err
	}

	if lspErr != nil || txt == "" {
		if lspErr != nil && s.bus != nil {
			s.bus.AddLSPError("hover", abs, line, char, lspErr)
		}
		word, _ := s.wordAt(abs, line, char)
		if word != "" && s.st != nil {
			units, _ := s.st.FindASTUnits(ctx, word, "", "", "", 1)
			if len(units) > 0 {
				u := units[0]
				fallbackTxt := fmt.Sprintf("Symbol: %s (%s)\n%s", u.Name, u.Kind, u.Signature)
				if u.Doc != "" {
					fallbackTxt += "\n\n" + u.Doc
				}
				warn := "Warning: LSP failed"
				if lspErr != nil {
					warn += fmt.Sprintf(" (%v)", lspErr)
				}
				warn += ", showing info from tree-sitter."
				return mcp.NewToolResultText(warn + "\n\n" + fallbackTxt), nil
			}
		}
		if lspErr != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Error: LSP failed (%v).", lspErr)), nil
		}
	}
	return mcp.NewToolResultText(txt), nil
}

func (s *LSPServer) handleImplementation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	if file == "" {
		return mcp.NewToolResultError("file is required"), nil
	}
	abs, err := s.resolveAbs(file)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	line := req.GetInt("line", 0)
	char := req.GetInt("character", 0)

	lspCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var lspErr error
	var locs []lsp.Location
	c, err := s.mgr.EnsureOpen(lspCtx, abs)
	if err == nil {
		locs, lspErr = c.Implementation(lspCtx, abs, line, char)
	} else {
		lspErr = err
	}

	if lspErr != nil {
		return s.fallbackToTreeSitter(ctx, "implementation", abs, line, char, lspErr)
	}
	return jsonResult(locs)
}

// fallbackToTreeSitter — общий fallback при ошибке LSP: ищем по слову в tree-sitter индексе.
func (s *LSPServer) fallbackToTreeSitter(ctx context.Context, toolName, abs string, line, char int, lspErr error) (*mcp.CallToolResult, error) {
	if s.bus != nil {
		s.bus.AddLSPError(toolName, abs, line, char, lspErr)
	}
	word, _ := s.wordAt(abs, line, char)
	if word != "" && s.st != nil {
		units, _ := s.st.FindASTUnits(ctx, word, "", "", "", 10)
		if len(units) > 0 {
			var fallbackLocs []lsp.Location
			for _, u := range units {
				fallbackLocs = append(fallbackLocs, lsp.Location{
					URI:       "file://" + u.FilePath,
					StartLine: u.StartLine - 1,
					StartChar: 0,
					EndLine:   u.StartLine - 1,
					EndChar:   0,
				})
			}
			data, _ := json.MarshalIndent(fallbackLocs, "", "  ")
			warn := fmt.Sprintf("Warning: LSP failed (%v), showing results from tree-sitter index.", lspErr)
			return mcp.NewToolResultText(warn + "\n" + string(data)), nil
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("Error: LSP failed (%v) and tree-sitter index found no results.", lspErr)), nil
}
