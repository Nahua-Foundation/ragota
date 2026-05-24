package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"aitools/internal/config"
	"aitools/internal/fileutil"
	"aitools/internal/lsp"
	"aitools/internal/state"
	"aitools/internal/store"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// LSPServer — MCP-обёртка над пулом LSP-клиентов (go/ts/python/java).
// Tools:
//   - lsp.definition(file, line, character)
//   - lsp.references(file, line, character, include_declaration?)
//   - lsp.hover(file, line, character)
//   - lsp.languages()
type LSPServer struct {
	cfg *config.Config
	mgr *lsp.Manager
	st  *store.SQLite
	bus *state.Bus
}

// NewLSPServer создаёт сервер.
func NewLSPServer(cfg *config.Config, mgr *lsp.Manager, st *store.SQLite, bus *state.Bus) *LSPServer {
	return &LSPServer{cfg: cfg, mgr: mgr, st: st, bus: bus}
}

func (s *LSPServer) Build() *server.MCPServer {
	srv := server.NewMCPServer("ai-tools-lsp", "0.1.0",
		server.WithToolCapabilities(false),
	)

	srv.AddTool(
		mcp.NewTool("lsp.definition",
			mcp.WithDescription("Go to definition for symbol at given position."),
			mcp.WithString("file", mcp.Required(), mcp.Description("File path.")),
			mcp.WithNumber("line", mcp.Required(), mcp.Description("0-based line.")),
			mcp.WithNumber("character", mcp.Required(), mcp.Description("0-based character.")),
		),
		s.wrap("lsp.definition", s.handleDefinition),
	)

	srv.AddTool(
		mcp.NewTool("lsp.references",
			mcp.WithDescription("Find references for symbol at given position."),
			mcp.WithString("file", mcp.Required()),
			mcp.WithNumber("line", mcp.Required()),
			mcp.WithNumber("character", mcp.Required()),
			mcp.WithBoolean("include_declaration", mcp.Description("Include the declaration itself."), mcp.DefaultBool(true)),
		),
		s.wrap("lsp.references", s.handleReferences),
	)

	srv.AddTool(
		mcp.NewTool("lsp.hover",
			mcp.WithDescription("Hover information at given position."),
			mcp.WithString("file", mcp.Required()),
			mcp.WithNumber("line", mcp.Required()),
			mcp.WithNumber("character", mcp.Required()),
		),
		s.wrap("lsp.hover", s.handleHover),
	)

	srv.AddTool(
		mcp.NewTool("lsp.languages",
			mcp.WithDescription("List configured LSP languages."),
		),
		s.wrap("lsp.languages", s.handleLanguages),
	)

	return srv
}

func (s *LSPServer) wrap(name string, fn func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := fn(ctx, req)
		if err != nil {
			return errorToResult(name, err)
		}
		if s.bus != nil {
			s.bus.IncMCPCall("lsp", name, false)
		}
		return res, nil
	}
}

func (s *LSPServer) resolveAbs(file string) (string, error) {
	return fileutil.SecureJoin(s.cfg.Root, file)
}

func (s *LSPServer) handleDefinition(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	if file == "" {
		return mcp.NewToolResultError("file is required"), nil
	}
	abs, err := s.resolveAbs(file)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	lang := fileutil.LanguageByExt(filepath.Ext(abs))

	line := int(req.GetFloat("line", 0))
	char := int(req.GetFloat("character", 0))

	// Используем background context с таймаутом для LSP операций,
	// чтобы LSP процесс не закрывался после завершения MCP запроса.
	// Увеличиваем таймаут до 60 секунд, так как JDTLS может инициализироваться долго.
	lspCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var lspErr error
	var locs []lsp.Location

	c, err := s.mgr.EnsureOpen(lspCtx, lang, abs)
	if err == nil {
		locs, lspErr = c.Definition(lspCtx, abs, line, char)
	} else {
		lspErr = err
	}

	// Fallback to tree-sitter index только при реальной ошибке LSP.
	// Пустой результат LSP — валидный ответ (символ без определения, например builtin),
	// и подмена его выдачей tree-sitter создаёт ложные срабатывания (например, в Java).
	if lspErr != nil {
		// Отправляем ошибку в bus для отображения в TUI
		if s.bus != nil {
			s.bus.AddLSPError("definition", abs, line, char, lspErr)
		}
		word, wordErr := s.wordAt(abs, line, char)
		if wordErr == nil && word != "" && s.st != nil {
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
		// Если treesitter ничего не нашёл — всё равно показываем ошибку LSP
		return mcp.NewToolResultText(fmt.Sprintf("Error: LSP failed (%v) and tree-sitter index found no results.", lspErr)), nil
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
	lang := fileutil.LanguageByExt(filepath.Ext(abs))

	line := int(req.GetFloat("line", 0))
	char := int(req.GetFloat("character", 0))
	includeDecl := req.GetBool("include_declaration", true)

	// Используем background context с таймаутом для LSP операций,
	// чтобы LSP процесс не закрывался после завершения MCP запроса.
	// Увеличиваем таймаут до 60 секунд, так как JDTLS может инициализироваться долго.
	lspCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var lspErr error
	var locs []lsp.Location

	c, err := s.mgr.EnsureOpen(lspCtx, lang, abs)
	if err == nil {
		locs, lspErr = c.References(lspCtx, abs, line, char, includeDecl)
	} else {
		lspErr = err
	}

	// Fallback to tree-sitter index при ошибке LSP
	if lspErr != nil {
		// Отправляем ошибку в bus для отображения в TUI
		if s.bus != nil {
			s.bus.AddLSPError("references", abs, line, char, lspErr)
		}
		word, _ := s.wordAt(abs, line, char)
		if word != "" && s.st != nil {
			units, _ := s.st.FindASTUnits(ctx, word, "", "", "", 10)
			if len(units) > 0 {
				// Ищем все упоминания символа в файлах (упрощённо — по имени)
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
		// Если treesitter ничего не нашёл — всё равно показываем ошибку LSP
		return mcp.NewToolResultText(fmt.Sprintf("Error: LSP failed (%v) and tree-sitter index found no results.", lspErr)), nil
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
	lang := fileutil.LanguageByExt(filepath.Ext(abs))

	line := int(req.GetFloat("line", 0))
	char := int(req.GetFloat("character", 0))

	// Используем background context с таймаутом для LSP операций,
	// чтобы LSP процесс не закрывался после завершения MCP запроса.
	// Увеличиваем таймаут до 60 секунд, так как JDTLS может инициализироваться долго.
	lspCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var lspErr error
	var txt string

	c, err := s.mgr.EnsureOpen(lspCtx, lang, abs)
	if err == nil {
		txt, lspErr = c.Hover(lspCtx, abs, line, char)
	} else {
		lspErr = err
	}

	if lspErr != nil || txt == "" {
		// Fallback to tree-sitter index
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
		// Если treesitter ничего не нашёл — всё равно показываем ошибку LSP
		if lspErr != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Error: LSP failed (%v) and tree-sitter index found no results.", lspErr)), nil
		}
	}

	return mcp.NewToolResultText(txt), nil
}

func (s *LSPServer) handleLanguages(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(s.mgr.Languages())
}

func (s *LSPServer) wordAt(file string, line, char int) (string, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(content), "\n")
	if line < 0 || line >= len(lines) {
		return "", fmt.Errorf("line out of range")
	}
	l := lines[line]
	if char < 0 || char >= len(l) {
		return "", fmt.Errorf("char out of range")
	}

	start := char
	for start > 0 && (unicode.IsLetter(rune(l[start-1])) || unicode.IsDigit(rune(l[start-1])) || l[start-1] == '_') {
		start--
	}
	end := char
	for end < len(l) && (unicode.IsLetter(rune(l[end])) || unicode.IsDigit(rune(l[end])) || l[end] == '_') {
		end++
	}
	return l[start:end], nil
}
