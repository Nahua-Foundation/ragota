// Package index содержит конкретные индексаторы: tree-sitter и vector.
// Они оба разделяют watcher/обходчик файлов и общую шину состояния state.Bus.
package index

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ragota/internal/config"
	"ragota/internal/fileutil"
	"ragota/internal/parser"
	"ragota/internal/state"
	"ragota/internal/store"
	"ragota/internal/watcher"
)

// TreeSitter — индексатор, сохраняющий символы файлов в SQLite.
// Может выгружать те же символы в Qdrant как payload-only, но это уже задача
// VectorIndexer; здесь — только SQLite.
type TreeSitter struct {
	cfg     *config.Config
	store   *store.SQLite
	parser  *parser.Parser
	bus     *state.Bus
	matcher *fileutil.Matcher

	mu sync.Mutex
}

// NewTreeSitter создаёт индексатор.
func NewTreeSitter(cfg *config.Config, st *store.SQLite, bus *state.Bus) *TreeSitter {
	return &TreeSitter{
		cfg:     cfg,
		store:   st,
		parser:  parser.New(),
		bus:     bus,
		matcher: fileutil.NewMatcher(cfg.Ignore),
	}
}

// FullScan делает полный обход root и индексирует все подходящие файлы.
func (t *TreeSitter) FullScan(ctx context.Context) error {
	if t.bus != nil {
		t.bus.SetIndexer("treesitter", func(i *state.Indexer) {
			i.Status = "scanning"
			i.FilesTotal = 0
			i.FilesIndexed = 0
			i.Symbols = 0
			i.LastError = ""
		})
	}

	// Сначала собираем список всех файлов для точного прогресса.
	var allFiles []string
	_ = fileutil.WalkFiles(t.cfg.Root, t.matcher, t.cfg.Extensions, func(abs, rel string, _ os.FileInfo) error {
		allFiles = append(allFiles, abs)
		return nil
	})

	total := len(allFiles)
	var indexed int
	var lastErr error

	for idx, abs := range allFiles {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := t.IndexFile(ctx, abs); err == nil {
			indexed++
		} else {
			lastErr = err
		}

		if t.bus != nil {
			t.bus.SetIndexer("treesitter", func(st *state.Indexer) {
				st.FilesTotal = total
				st.FilesIndexed = idx + 1
				st.Status = "indexing"
				if lastErr != nil {
					st.LastError = lastErr.Error()
				}
			})
		}
	}

	if t.bus != nil {
		t.bus.SetIndexer("treesitter", func(i *state.Indexer) {
			if lastErr != nil && indexed < total {
				i.Status = "error"
				i.LastError = lastErr.Error()
			} else {
				i.Status = "idle"
			}
		})
		if st, e := t.store.Stats(ctx); e == nil {
			t.bus.SetIndexer("treesitter", func(i *state.Indexer) {
				i.Symbols = st.Symbols
				i.FilesTotal = total
				i.FilesIndexed = total
			})
		}
	}
	return nil
}

// IndexFile парсит один файл и записывает его символы.
func (t *TreeSitter) IndexFile(ctx context.Context, abs string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	start := time.Now()
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	hash, err := fileutil.HashFile(abs)
	if err != nil {
		return err
	}

	// Skip, если хэш не поменялся
	prev, _ := t.store.GetFile(ctx, abs)
	if prev != nil && prev.Hash == hash {
		return nil
	}

	src, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	lang := fileutil.LanguageByExt(filepath.Ext(abs))
	syms, _ := t.parser.Parse(ctx, lang, abs, src)

	rows := make([]store.SymbolRow, 0, len(syms))
	for _, s := range syms {
		rows = append(rows, store.SymbolRow{
			Name:       s.Name,
			Kind:       s.Kind,
			StartLine:  s.StartLine,
			EndLine:    s.EndLine,
			StartByte:  s.StartByte,
			EndByte:    s.EndByte,
			ParentName: s.Parent,
			Signature:  s.Signature,
		})
	}
	if err := t.store.UpsertFile(ctx, store.FileRow{
		Path:     abs,
		Language: lang,
		Hash:     hash,
		Size:     info.Size(),
		ModTime:  info.ModTime(),
	}, rows); err != nil {
		return err
	}

	if t.bus != nil {
		rel, _ := filepath.Rel(t.cfg.Root, abs)
		t.bus.AddRecent(state.FileEntry{
			Path:       rel,
			Kind:       "treesitter",
			Symbols:    len(rows),
			DurationMs: time.Since(start).Milliseconds(),
		})
	}
	return nil
}

// RemoveFile удаляет файл из индекса.
func (t *TreeSitter) RemoveFile(ctx context.Context, abs string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.store.DeleteFile(ctx, abs)
}

// Watch запускает наблюдение и применяет изменения к индексу.
// Возвращает, когда ctx завершён.
func (t *TreeSitter) Watch(ctx context.Context, w *watcher.Watcher) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.Events():
			if !ok {
				return nil
			}
			switch ev.Kind {
			case watcher.EventRemove, watcher.EventRename:
				_ = t.RemoveFile(ctx, ev.AbsPath)
			default:
				_ = t.IndexFile(ctx, ev.AbsPath)
			}
		}
	}
}
