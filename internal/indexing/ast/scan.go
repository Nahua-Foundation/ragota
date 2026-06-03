package astindex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"ragota/pkg/fileutil"
	"ragota/pkg/state"
	"ragota/internal/store"
)

// pendingFile хранит результат парсинга одного файла для второй фазы.
type pendingFile struct {
	path   string
	repo   string
	hash   string
	units  []store.ASTUnit
	edges  []pendingEdge
}

// symbolEntry — запись в in-memory map для резолва edges.
type symbolEntry struct {
	id       int  // будет проставлен после записи в БД
	qualified string
	name     string
	language string
	repo     string
}

// FullScan — двухпроходная индексация:
//
//   Pass 1 (сбор): парсим изменённые файлы, собираем units+edges в память,
//                  строим map[qualified_name] → symbolEntry.
//   Pass 2 (резолв): для каждого файла разрешаем dst_id через in-memory map
//                  (без SQL JOIN), пишем в БД батчами.
//
// Инкрементальность: если хэш файла не изменился — skip.
func (i *Indexer) FullScan(ctx context.Context) error {
	if i.bus != nil {
		i.bus.SetIndexer("ast", func(st *state.Indexer) {
			st.Status = "scanning"
			st.FilesTotal = 0
			st.FilesIndexed = 0
			st.Symbols = 0
			st.LastError = ""
		})
	}

	// Detect stale hashes: SQLite has file hashes but graph tables are empty.
	if i.st != nil {
		stats, _ := i.st.GraphStats(ctx)
		if stats.Units == 0 {
			hasHashes, _ := i.st.HasFileHashes(ctx)
			if hasHashes {
				_ = i.st.ResetFileHashes(ctx)
			}
			// Also reset cross_hash if graph is empty but cross_hash exists
			hasCrossHash, _ := i.st.HasCrossHashes(ctx)
			if hasCrossHash {
				_ = i.st.ResetCrossHashes(ctx)
			}
		}
	}

	var allFiles []string
	_ = fileutil.WalkFiles(i.cfg.Root, i.matcher, i.cfg.Extensions, func(abs, rel string, _ os.FileInfo) error {
		allFiles = append(allFiles, abs)
		return nil
	})

	total := len(allFiles)
	if i.bus != nil {
		i.bus.SetIndexer("ast", func(st *state.Indexer) {
			st.FilesTotal = total
		})
	}

	// ═══ PASS 1: Parse changed files, collect in memory ═══
	var pending []pendingFile
	symbolMap := make(map[string]*symbolEntry, total*20) // ~20 symbols/file
	var indexed int
	var skipped int
	var lastErr error

	for idx, abs := range allFiles {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		src, err := os.ReadFile(abs)
		if err != nil {
			lastErr = err
			skipped++
			continue
		}
		hash := fileutil.HashBytes(src)
		prevHash, _ := i.st.GetFileHash(ctx, abs)
		if prevHash != "" && prevHash == hash {
			skipped++
			// Даже для skipped файлов добавляем в symbolMap из БД
			// (чтобы edges из других файлов могли на них сослаться)
			i.addToSymbolMap(ctx, abs, symbolMap)
			i.updateProgress(idx+1, total, lastErr)
			continue
		}

		units, edges, err := i.parseFile(ctx, abs, src)
		if err != nil {
			lastErr = err
			i.updateProgress(idx+1, total, lastErr)
			continue
		}

		var repo string
		if i.resolv != nil {
			repo = i.resolv.For(abs)
		}
		for k := range units {
			units[k].Repo = repo
		}

		pending = append(pending, pendingFile{
			path:  abs,
			repo:  repo,
			hash:  hash,
			units: units,
			edges: edges,
		})
		indexed++

		// Добавляем новые units в symbolMap (с repo-префиксом для multi-repo)
		for _, u := range units {
			key := u.Qualified
			if key == "" {
				key = u.Name
			}
			if key != "" {
				entry := &symbolEntry{
					qualified: u.Qualified,
					name:      u.Name,
					language:  u.Language,
					repo:      u.Repo,
				}
				// ОДИН entry для всех ключей
				symbolMap[key] = entry
				if u.Repo != "" {
					symbolMap[u.Repo+"/"+key] = entry
				}
				// Также добавляем по имени (без qualified) — для вызовов вида Save()
				if u.Name != "" && u.Name != key {
					symbolMap[u.Name] = entry
					if u.Repo != "" {
						symbolMap[u.Repo+"/"+u.Name] = entry
					}
				}
			}
		}

		i.updateProgress(idx+1, total, lastErr)
	}

	// ═══ PASS 2: Write units first, resolve edges, then write edges ═══

	// Phase 2a: Write all units to DB, get real IDs
	// fileUnitIDs хранит: path → []int (SrcUnitIdx → real DB ID)
	// parentUpdates хранит: path → []parentUpdate{childDBID, parentDBID}
	// Инкрементальные счётчики для быстрого отображения без COUNT(*)
	var totalUnits, totalEdges int

	fileUnitIDs := make(map[string][]int, len(pending))
	var parentUpdates []struct {
		path     string
		childID  int
		parentID int
	}

	for _, pf := range pending {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Extract parent_id before insert (index-based → will be resolved after insert)
		// Save parent mappings and clear parent_id in units to avoid FK violation
		idxToParent := make(map[int]int, len(pf.units))
		for idx, u := range pf.units {
			if u.ParentID.Valid {
				idxToParent[idx] = int(u.ParentID.Int64)
				pf.units[idx].ParentID = sql.NullInt64{}
			}
		}

		// Ensure file exists in files table (FK: file_path REFERENCES files(path))
		lang := detectLang(pf.path)
		if err := i.st.EnsureFile(ctx, pf.path, lang); err != nil {
			lastErr = fmt.Errorf("astindex: ensure file: %w", err)
			continue
		}

		// Записываем units с parent_id=NULL
		ids, unitIDs, err := i.st.ReplaceFileGraph(ctx, pf.path, pf.units, nil, pf.repo)
		if err != nil {
			lastErr = err
			continue
		}

		totalUnits += len(pf.units)
		fileUnitIDs[pf.path] = unitIDs

		// Collect parent updates (idxToParent uses unit indices, resolve to DB IDs)
		for childIdx, parentIdx := range idxToParent {
			if parentIdx >= 0 && parentIdx < len(unitIDs) && childIdx < len(unitIDs) {
				parentUpdates = append(parentUpdates, struct {
					path     string
					childID  int
					parentID int
				}{pf.path, unitIDs[childIdx], unitIDs[parentIdx]})
			}
		}

		// Обновляем symbolMap реальными ID
		for key, dbID := range ids {
			if entry, ok := symbolMap[key]; ok {
				entry.id = dbID
			}
		}
		// Подгружаем все units для completeness (если entry ещё не создан в Phase 1)
		persisted, _ := i.st.ListASTUnitsByFile(ctx, pf.path)
		for _, u := range persisted {
			key := u.Qualified
			if key == "" {
				key = u.Name
			}
			if key != "" {
				if entry, ok := symbolMap[key]; ok {
					entry.id = u.ID
				} else {
					entry := &symbolEntry{
						id:       u.ID,
						qualified: u.Qualified,
						name:     u.Name,
						language: u.Language,
						repo:     u.Repo,
					}
					symbolMap[key] = entry
					if u.Repo != "" {
						symbolMap[u.Repo+"/"+key] = entry
					}
					// Также по имени
					if u.Name != "" && u.Name != key {
						symbolMap[u.Name] = entry
						if u.Repo != "" {
							symbolMap[u.Repo+"/"+u.Name] = entry
						}
					}
				}
			}
		}

		// Обновляем хэш файла
		if err := i.st.UpdateFileHash(ctx, pf.path, pf.hash); err != nil {
			lastErr = fmt.Errorf("astindex: update file hash: %w", err)
		}
	}

	// Apply parent_id updates (after all units have real DB IDs)
	if len(parentUpdates) > 0 {
		updates := make(map[int]int, len(parentUpdates))
		for _, pu := range parentUpdates {
			updates[pu.childID] = pu.parentID
		}
		if err := i.st.UpdateASTParents(ctx, updates); err != nil {
			lastErr = fmt.Errorf("astindex: update parents: %w", err)
		}
	}

	// Phase 2b: Resolve edges via symbolMap, write edges
	resolvedCount := 0
	unresolvedCount := 0

	// Счётчик файлов в Phase 2b — для отображения процентов
	phase2Files := 0

	for _, pf := range pending {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		edgeRefs := make([]store.EdgeRef, 0, len(pf.edges))
		for _, e := range pf.edges {
			ref := store.EdgeRef{
				SrcUnitIdx: e.srcIdx,
				DstName:    e.dstName,
				DstUnitIdx: e.dstIdx,
				Kind:       e.kind,
				Line:       e.line,
			}

			// Разрешаем src_id из fileUnitIDs
			unitIDs := fileUnitIDs[pf.path]
			if e.srcIdx >= 0 && e.srcIdx < len(unitIDs) {
				ref.ResolvedSrcID = unitIDs[e.srcIdx]
			}

			// Пытаемся разрешить dst_id через symbolMap
			if e.dstIdx < 0 && e.dstName != "" {
				// Сначала ищем по repo/name (точное совпадение в той же репе)
				repoKey := pf.repo + "/" + e.dstName
				if entry, ok := symbolMap[repoKey]; ok {
					if entry.language == "" || entry.language == detectLang(pf.path) {
						ref.ResolvedDstID = entry.id
						resolvedCount++
						goto edgeResolved
					}
				}
				// Fallback: ищем по имени (без repo)
				if entry, ok := symbolMap[e.dstName]; ok {
					if entry.language == "" || entry.language == detectLang(pf.path) {
						ref.ResolvedDstID = entry.id
						resolvedCount++
					} else {
						unresolvedCount++
					}
				} else {
					unresolvedCount++
				}
			} else if e.dstIdx >= 0 {
				resolvedCount++
			}
		edgeResolved:

			edgeRefs = append(edgeRefs, ref)
		}

		// Записываем edges через ReplaceFileGraph (units уже записаны, передаём пустые units)
		if len(edgeRefs) > 0 {
			_, _, err := i.st.ReplaceFileGraph(ctx, pf.path, nil, edgeRefs, pf.repo)
			if err != nil {
				lastErr = err
				continue
			}
			totalEdges += len(edgeRefs)
		}

		// Обновляем прогресс для TUI (проценты в Phase 2b)
		phase2Files++
		if i.bus != nil {
			i.bus.SetIndexer("ast", func(st *state.Indexer) {
				st.Status = "resolving"
				st.FilesIndexed = phase2Files
				st.FilesTotal = len(pending)
			})
		}

		// Report to bus
		if i.bus != nil {
			i.bus.AddRecent(state.FileEntry{
				Path:    pf.path,
				Kind:    "ast",
				Symbols: len(pf.units),
			})
		}
	}

	_ = resolvedCount
	_ = unresolvedCount

	// ═══ Финал ═══
	// При indexed > 0 используем инкрементальные счётчики (быстро).
	// При indexed == 0 (все skipped) — загружаем из БД, чтобы показать
	// реальные цифры уже проиндексированных данных.
	symbols := totalUnits
	edges := totalEdges
	if indexed == 0 {
		gs, err := i.st.GraphStats(ctx)
		if err == nil {
			symbols = gs.Units
			edges = gs.Edges
		}
	}
	if lastErr == nil && i.bus != nil {
		i.bus.SetIndexer("ast", func(st *state.Indexer) {
			st.Status = "idle"
			st.FilesTotal = total
			st.FilesIndexed = total
			st.Symbols = symbols
			st.Chunks = edges
		})
	} else if i.bus != nil {
		i.bus.SetIndexer("ast", func(st *state.Indexer) {
			st.Status = "error"
			st.LastError = lastErr.Error()
			st.FilesTotal = total
			st.FilesIndexed = total
		})
	}

	return lastErr
}

// addToSymbolMap загружает symbols файла из БД в memory map.
func (i *Indexer) addToSymbolMap(ctx context.Context, path string, m map[string]*symbolEntry) {
	units, err := i.st.ListASTUnitsByFile(ctx, path)
	if err != nil {
		return
	}
	// Определяем repo файла
	var repo string
	if i.resolv != nil {
		repo = i.resolv.For(path)
	}
	for _, u := range units {
		key := u.Qualified
		if key == "" {
			key = u.Name
		}
		if key != "" {
			entry := &symbolEntry{
				id:       u.ID,
				qualified: u.Qualified,
				name:     u.Name,
				language: u.Language,
				repo:     repo,
			}
			m[key] = entry
			if repo != "" {
				m[repo+"/"+key] = entry
			}
			// Также по имени
			if u.Name != "" && u.Name != key {
				m[u.Name] = entry
				if repo != "" {
					m[repo+"/"+u.Name] = entry
				}
			}
		}
	}
}

// updateProgress обновляет статус в bus.
func (i *Indexer) updateProgress(indexed, total int, lastErr error) {
	if i.bus != nil {
		i.bus.SetIndexer("ast", func(st *state.Indexer) {
			st.FilesIndexed = indexed
			if indexed < total {
				st.Status = "indexing"
			} else {
				st.Status = "resolving"
			}
			if lastErr != nil {
				st.LastError = lastErr.Error()
			}
		})
	}
}

// indexFile — внутренняя реализация для тестов.
func (i *Indexer) indexFile(ctx context.Context, path string, resolveEdges bool) error {
	if i == nil || i.st == nil {
		return nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	hash := fileutil.HashBytes(src)
	return i.indexFileWithHash(ctx, path, src, hash, resolveEdges)
}

// indexFileWithHash — вариант indexFile, который принимает уже прочитанный
// src и hash файла. После успешной записи обновляет files.hash.
func (i *Indexer) indexFileWithHash(ctx context.Context, path string, src []byte, hash string, resolveEdges bool) error {
	if i == nil || i.st == nil {
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

	if err := i.st.UpdateFileHash(ctx, path, hash); err != nil {
		return fmt.Errorf("astindex: update file hash: %w", err)
	}

	if resolveEdges {
		if _, err := i.st.ResolvePendingEdges(ctx, nil); err != nil {
			return fmt.Errorf("astindex: resolve pending edges: %w", err)
		}
	}

	if i.bus != nil {
		i.bus.AddRecent(state.FileEntry{
			Path:       path,
			Kind:       "ast",
			Symbols:    len(units),
			DurationMs: time.Since(start).Milliseconds(),
		})
	}

	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
