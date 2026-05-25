// Package astindex — индексатор AST units и code-graph edges.
//
// Для Go используется стандартная библиотека go/parser + go/ast: это даёт
// точную информацию о функциях/методах/типах/импортах/вызовах в пределах
// одного файла без построения полного go/packages-графа (что было бы
// тяжеловесно и требовало рабочего go-окружения).
//
// Для остальных языков (TS/JS/Python/Java) AST units заполняются на основе
// существующего tree-sitter symbols (см. internal/parser): edges пока не
// извлекаются, что соответствует согласованной стратегии «Go-first».
//
// Сохранение в БД — через store.ReplaceASTUnits + store.ReplaceEdges
// (атомарно по файлу). Разрешение dst_id у edges выполняется отложенно
// через store.ResolvePendingEdges после полного скана.
//
// Реализация декомпозирована по доменам:
//
//   - astindex.go            — публичный API Indexer (New/SetBus/IndexFile/
//     FullScan/RemoveFile) и общая оркестрация записи в БД;
//   - parse_go.go            — Go-специфичный экстрактор (go/ast),
//     pendingEdge + addFuncDecl + addGenDecl;
//   - parse_generic.go       — generic-экстрактор поверх tree-sitter;
//   - treesitter_extractor.go — extractor для Java/TS/JS с edges
//     (parseWithTreeSitter, см. отдельный файл);
//   - util.go                — мелкие чистые helper'ы (detectLang/exprName/
//     signatureOf/firstLine/commentText/hashBytes).
package astindex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ragota/internal/config"
	"ragota/internal/fileutil"
	pkgparser "ragota/internal/parser"
	"ragota/internal/repos"
	"ragota/internal/state"
	"ragota/internal/store"
)

// Indexer — индексатор AST units и edges.
//
// В multi-repo workspace индексатор получает резолвер репо
// (repos.Resolver) и проставляет поле Repo каждой AST-единицы и ребра по
// prefix-match абсолютного пути файла к одному из известных репо.
// Если резолвер пуст (nil) или путь не соответствует ни одной репе —
// repo остаётся пустым (обратная совместимость со старым single-root
// поведением).
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
		matcher: fileutil.NewMatcher(cfg.Ignore),
	}
}

// SetRepoResolver подключает резолвер репозиториев. Если не вызван,
// индексатор ведёт себя как раньше (одна анонимная репа = "").
func (i *Indexer) SetRepoResolver(r *repos.Resolver) { i.resolv = r }

// SetBus устанавливает шину событий для статистики.
func (i *Indexer) SetBus(bus *state.Bus) {
	i.bus = bus
}

// IndexFile парсит файл, извлекает AST units + edges и сохраняет в SQLite.
// path должен быть абсолютным. Сразу резолвит отложенные dst_id у edges —
// предназначен для инкрементальной переиндексации (watcher / явный reindex).
func (i *Indexer) IndexFile(ctx context.Context, path string) error {
	return i.indexFile(ctx, path, true)
}

// indexFile — внутренняя реализация. resolveEdges=false используется в FullScan,
// чтобы избежать N лишних вызовов ResolvePendingEdges на каждый файл; финальный
// резолв выполняется один раз в конце скана.
func (i *Indexer) indexFile(ctx context.Context, path string, resolveEdges bool) error {
	if i == nil || i.st == nil {
		return nil
	}
	start := time.Now()
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lang := detectLang(path)

	var (
		units []store.ASTUnit
		edges []pendingEdge
	)
	switch lang {
	case "go":
		units, edges, err = i.parseGo(path, src)
	case "java", "typescript", "javascript":
		units, edges, err = i.parseWithTreeSitter(ctx, lang, path, src)
		// fileFromNode возвращает пусто — заполним FilePath здесь.
		for k := range units {
			if units[k].FilePath == "" {
				units[k].FilePath = path
			}
		}
	default:
		units, err = i.parseGeneric(ctx, lang, path, src)
	}
	if err != nil {
		return err
	}

	// Определяем репу, к которой относится файл (multi-repo workspace).
	// Если резолвер не подключён или путь не покрывается ни одной репой —
	// repo = "" (legacy single-root режим).
	var repo string
	if i.resolv != nil {
		repo = i.resolv.For(path)
	}
	// Проставляем repo всем юнитам файла.
	for k := range units {
		units[k].Repo = repo
	}

	// Запись units. parent_id здесь — индексный (0-based) reference на
	// предыдущие units в этом же файле; пересчитаем после получения
	// реальных ids.
	rel, _ := filepath.Rel(i.cfg.Root, path)
	_ = rel

	// Сначала сохраняем все units без parent_id (parent проставим вторым
	// проходом), чтобы получить реальные ids.
	idxToParent := make(map[int]int, len(units))
	for i, u := range units {
		if u.ParentID.Valid {
			idxToParent[i] = int(u.ParentID.Int64)
			units[i].ParentID = sql.NullInt64{} // сбрасываем — это «индекс», не id
		}
	}

	// Гарантируем наличие файла в таблице files (внешний ключ ast_units.file_path).
	if err := i.st.EnsureFile(ctx, path, lang); err != nil {
		return fmt.Errorf("astindex: ensure file: %w", err)
	}

	idMap, err := i.st.ReplaceASTUnits(ctx, path, units)
	if err != nil {
		return fmt.Errorf("astindex: replace units: %w", err)
	}

	// Второй проход: проставляем parent_id по реальным ids.
	// Узнаём текущие ids (порядок такой же, как при вставке).
	persisted, err := i.st.ListASTUnitsByFile(ctx, path)
	if err != nil {
		return err
	}
	if len(persisted) == len(units) {
		// Соберём апдейты parent_id одной транзакцией: проще выгрузить
		// заново. Здесь — fallback на чистый exec из store, через простой
		// проход (доп. метод в store: UpdateASTParents).
		updates := make(map[int64]int64, len(idxToParent))
		for idx, parentIdx := range idxToParent {
			if parentIdx < 0 || parentIdx >= len(persisted) {
				continue
			}
			updates[persisted[idx].ID] = persisted[parentIdx].ID
		}
		if err := i.st.UpdateASTParents(ctx, updates); err != nil {
			return err
		}
	}

	// Теперь — edges. У edges src указывается по «локальному индексу»
	// исходного unit'а, dst либо по индексу (если внутри файла), либо по
	// имени (qualified) для отложенного резолва.
	resolvedEdges := make([]store.Edge, 0, len(edges))
	for _, e := range edges {
		if e.srcIdx < 0 || e.srcIdx >= len(persisted) {
			continue
		}
		ed := store.Edge{
			Repo:     repo,
			SrcID:    persisted[e.srcIdx].ID,
			Kind:     e.kind,
			DstName:  e.dstName,
			FilePath: path,
			Line:     e.line,
		}
		if e.dstIdx >= 0 && e.dstIdx < len(persisted) {
			ed.DstID = persisted[e.dstIdx].ID
		} else if e.dstName != "" {
			// dst_id=0 — будет разрешён ResolvePendingEdges.
			if id, ok := idMap[e.dstName]; ok {
				ed.DstID = id
			}
		}
		resolvedEdges = append(resolvedEdges, ed)
	}
	if err := i.st.ReplaceEdges(ctx, path, resolvedEdges); err != nil {
		return fmt.Errorf("astindex: replace edges: %w", err)
	}

	// Резолвим dst_id для новых рёбер сразу после записи: это важно для
	// инкрементальной переиндексации (watcher / явный reindex одного файла),
	// иначе find_references / find_callers / find_implementations /
	// expand_neighbors остаются пустыми до полного FullScan.
	// В режиме FullScan (resolveEdges=false) пропускаем — там один общий
	// финальный вызов ResolvePendingEdges.
	if resolveEdges {
		if _, err := i.st.ResolvePendingEdges(ctx); err != nil {
			return fmt.Errorf("astindex: resolve pending edges: %w", err)
		}
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

// FullScan индексирует все подходящие файлы под cfg.Root и затем разрешает
// отложенные dst_id у edges одним финальным вызовом ResolvePendingEdges.
// Per-file резолв отключён для производительности — на крупных проектах это
// убирает N лишних запросов к SQLite.
func (i *Indexer) FullScan(ctx context.Context) error {
	if i.bus != nil {
		i.bus.SetIndexer("graph", func(st *state.Indexer) {
			st.Status = "scanning"
			st.LastError = ""
		})
	}

	var allFiles []string
	_ = fileutil.WalkFiles(i.cfg.Root, i.matcher, i.cfg.Extensions, func(abs, rel string, _ os.FileInfo) error {
		allFiles = append(allFiles, abs)
		return nil
	})

	total := len(allFiles)
	var indexed int
	var lastErr error

	// Страховка на случай раннего выхода (ctx cancel / panic recover в
	// вызывающем коде): разрешаем накопленные pending edges, чтобы граф
	// не остался полу-связным. Безопасно вызывать многократно.
	defer func() {
		// Используем context.Background, т.к. ctx может быть уже отменён.
		if _, err := i.st.ResolvePendingEdges(context.Background()); err != nil && lastErr == nil {
			lastErr = err
		}
	}()

	for idx, abs := range allFiles {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := i.indexFile(ctx, abs, false); err == nil {
			indexed++
		} else {
			lastErr = err
		}

		if i.bus != nil {
			i.bus.SetIndexer("graph", func(st *state.Indexer) {
				st.FilesTotal = total
				st.FilesIndexed = idx + 1
				st.Status = "indexing"
				if lastErr != nil {
					st.LastError = lastErr.Error()
				}
			})
		}
	}

	if i.bus != nil {
		i.bus.SetIndexer("graph", func(st *state.Indexer) {
			st.Status = "resolving"
		})
	}
	if _, err := i.st.ResolvePendingEdges(ctx); err != nil {
		lastErr = err
	}

	if i.bus != nil {
		i.bus.SetIndexer("graph", func(st *state.Indexer) {
			if lastErr != nil && indexed < total {
				st.Status = "error"
				st.LastError = lastErr.Error()
			} else {
				st.Status = "idle"
			}
			st.FilesTotal = total
			st.FilesIndexed = total
		})
		if gs, err := i.st.GraphStats(ctx); err == nil {
			i.bus.SetIndexer("graph", func(st *state.Indexer) {
				st.Symbols = gs.Units
				st.Chunks = gs.Edges // Используем Chunks для Edges
			})
		}
	}
	return lastErr
}

// RemoveFile удаляет AST units и edges для файла.
func (i *Indexer) RemoveFile(ctx context.Context, path string) error {
	if _, err := i.st.ReplaceASTUnits(ctx, path, nil); err != nil {
		return err
	}
	return i.st.ReplaceEdges(ctx, path, nil)
}
