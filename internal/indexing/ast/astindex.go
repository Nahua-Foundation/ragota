// Package astindex — индексатор AST units и code-graph edges.
//
// Для Go используется стандартная библиотека go/parser + go/ast.
// Для остальных языков (TS/JS/Python/Java) — tree-sitter.
//
// Структура файлов:
//   - astindex.go            — Indexer struct, New, setters, IndexFile, RemoveFile.
//   - scan.go                — FullScan, indexFileWithHash.
//   - save.go                — saveUnitsAndEdges, parseFile (общее ядро записи).
//   - parse_go.go            — Go-специфичный экстрактор (go/ast).
//   - parse_generic.go       — generic-экстрактор поверх tree-sitter.
//   - treesitter_extractor.go — extractor для Java/TS/JS с edges.
//   - util.go                — мелкие helper'ы (detectLang/exprName/...).
package astindex

import (
	"context"
	"os"
	"time"

	"ragota/pkg/config"
	"ragota/pkg/fileutil"
	pkgparser "ragota/internal/indexing/parser"
	"ragota/pkg/repos"
	"ragota/pkg/state"
	"ragota/internal/store"
)

// Indexer — индексатор AST units и edges.
//
// В multi-repo workspace индексатор получает резолвер репо
// (repos.Resolver) и проставляет поле Repo каждой AST-единицы и ребра.
type Indexer struct {
	cfg     *config.Config
	st      *store.SQLite
	ts      *pkgparser.Parser
	bus     *state.Bus
	matcher *fileutil.Matcher
	resolv  *repos.Resolver
}

// New создаёт индексатор.
func New(cfg *config.Config, st *store.SQLite) *Indexer {
	return &Indexer{
		cfg:     cfg,
		st:      st,
		ts:      pkgparser.New(),
		matcher: fileutil.NewMatcher(cfg.IgnorePatterns),
	}
}

// SetRepoResolver подключает резолвер репозиториев.
func (i *Indexer) SetRepoResolver(r *repos.Resolver) { i.resolv = r }

// SetBus устанавливает шину событий для статистики.
func (i *Indexer) SetBus(bus *state.Bus) {
	i.bus = bus
}

// IndexFile парсит файл, извлекает AST units + edges и сохраняет в SQLite.
// Если хэш файла не изменился с предыдущей индексации, файл пропускается.
func (i *Indexer) IndexFile(ctx context.Context, path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	hash := fileutil.HashBytes(src)
	prevHash, _ := i.st.GetFileHash(ctx, path)
	if prevHash != "" && prevHash == hash {
		return nil
	}

	start := time.Now()

	units, edges, err := i.parseFile(ctx, path, src)
	if err != nil {
		return err
	}

	var repo string
	if i.resolv != nil {
		repo = i.resolv.For(path)
	}

	if err := i.saveUnitsAndEdges(ctx, path, units, edges, repo, nil); err != nil {
		return err
	}

	if _, err := i.st.ResolvePendingEdges(ctx, nil); err != nil {
		return err
	}

	if i.bus != nil {
		i.bus.AddRecent(state.FileEntry{
			Path:       path,
			Kind:       "graph",
			Symbols:    len(units),
			DurationMs: time.Since(start).Milliseconds(),
		})
	}

	return nil
}

// RemoveFile удаляет AST units и edges для файла.
func (i *Indexer) RemoveFile(ctx context.Context, path string) error {
	return i.st.RemoveFileGraph(ctx, path)
}
