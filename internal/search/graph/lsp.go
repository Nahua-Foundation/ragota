package graph

import (
	"context"
	"path/filepath"
	"time"

	"ragota/internal/store"
	"ragota/pkg/fileutil"
	"ragota/pkg/logger"
	"ragota/pkg/lsp"
)

// lspCallers выполняет textDocument/references на позиции определения функции
// и резолвит локации обратно в AST units. При любой ошибке возвращает nil
// (fallback на tree-sitter обеспечивается вызывающим).
func (s *Service) lspCallers(ctx context.Context, unitID int) []store.ASTUnit {
	if s.mgr == nil {
		logger.Log().Debug().Int("unit_id", unitID).Msg("lsp.callers: manager not configured")
		return nil
	}
	s.mu.Lock()
	// lazy eviction — remove expired entries on read
	if e, ok := s.callCache[unitID]; ok {
		if time.Since(e.at) < cacheTTL {
			s.mu.Unlock()
			return e.units
		}
		delete(s.callCache, unitID)
	}
	// cap cache size — evict oldest entry if at capacity
	if len(s.callCache) >= cacheMaxSize {
		var oldestKey int
		oldestAt := time.Now()
		for k, e := range s.callCache {
			if e.at.Before(oldestAt) {
				oldestAt = e.at
				oldestKey = k
			}
		}
		if oldestKey != 0 || len(s.callCache) > 0 {
			delete(s.callCache, oldestKey)
		}
	}
	s.mu.Unlock()

	// use background context for LSP cache-fill — caller ctx may be cancelled
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	u, err := s.st.GetASTUnit(cctx, unitID)
	if err != nil || u == nil {
		logger.Log().Debug().Int("unit_id", unitID).Err(err).Msg("lsp.callers: GetASTUnit failed")
		return nil
	}
	lang := fileutil.LanguageByExt(filepath.Ext(u.FilePath))
	if lang == "" {
		logger.Log().Debug().Str("file", u.FilePath).Msg("lsp.callers: unknown language")
		return nil
	}
	cli, err := s.mgr.EnsureOpen(cctx, u.FilePath)
	if err != nil || cli == nil {
		logger.Log().Debug().Str("file", u.FilePath).Err(err).Msg("lsp.callers: EnsureOpen failed")
		return nil
	}
	locs, err := cli.References(cctx, u.FilePath, max0(u.StartLine-1), nameColumn(u.Signature, u.Name), false)
	if err != nil || len(locs) == 0 {
		logger.Log().Debug().Str("file", u.FilePath).Str("symbol", u.Name).
			Int("line", u.StartLine).Err(err).Int("locations", len(locs)).
			Msg("lsp.callers: References returned empty")
		return nil
	}
	logger.Log().Debug().Int("unit_id", unitID).Int("locations", len(locs)).Msg("lsp.callers: found locations")
	units := s.locationsToUnits(cctx, locs, true /* enclosing */)
	s.mu.Lock()
	s.callCache[unitID] = cacheEntry{units: units, at: time.Now()}
	s.mu.Unlock()
	return units
}

// lspReferences выполняет textDocument/references и возвращает enclosing units.
// Используется для обогащения find_references когда tree-sitter edges недостаточно.
func (s *Service) lspReferences(ctx context.Context, unitID int) []store.ASTUnit {
	if s.mgr == nil {
		logger.Log().Debug().Int("unit_id", unitID).Msg("lsp.references: manager not configured")
		return nil
	}
	s.mu.Lock()
	// lazy eviction
	if e, ok := s.refCache[unitID]; ok {
		if time.Since(e.at) < cacheTTL {
			s.mu.Unlock()
			return e.units
		}
		delete(s.refCache, unitID)
	}
	// cap cache size
	if len(s.refCache) >= cacheMaxSize {
		var oldestKey int
		oldestAt := time.Now()
		for k, e := range s.refCache {
			if e.at.Before(oldestAt) {
				oldestAt = e.at
				oldestKey = k
			}
		}
		if oldestKey != 0 || len(s.refCache) > 0 {
			delete(s.refCache, oldestKey)
		}
	}
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	u, err := s.st.GetASTUnit(cctx, unitID)
	if err != nil || u == nil {
		logger.Log().Debug().Int("unit_id", unitID).Err(err).Msg("lsp.references: GetASTUnit failed")
		return nil
	}
	lang := fileutil.LanguageByExt(filepath.Ext(u.FilePath))
	if lang == "" {
		logger.Log().Debug().Str("file", u.FilePath).Msg("lsp.references: unknown language")
		return nil
	}
	cli, err := s.mgr.EnsureOpen(cctx, u.FilePath)
	if err != nil || cli == nil {
		logger.Log().Debug().Str("file", u.FilePath).Err(err).Msg("lsp.references: EnsureOpen failed")
		return nil
	}
	// includeDecl=false — не включаем само определение
	locs, err := cli.References(cctx, u.FilePath, max0(u.StartLine-1), nameColumn(u.Signature, u.Name), false)
	if err != nil || len(locs) == 0 {
		logger.Log().Debug().Str("file", u.FilePath).Str("symbol", u.Name).
			Int("line", u.StartLine).Err(err).Int("locations", len(locs)).
			Msg("lsp.references: References returned empty")
		return nil
	}
	// enclosing=true — возвращаем функции/методы, содержащие ссылку
	units := s.locationsToUnits(cctx, locs, true)
	s.mu.Lock()
	s.refCache[unitID] = cacheEntry{units: units, at: time.Now()}
	s.mu.Unlock()
	return units
}

// lspImplementations выполняет textDocument/implementation для интерфейса и
// возвращает AST units реализаций.
func (s *Service) lspImplementations(ctx context.Context, interfaceID int) []store.ASTUnit {
	if s.mgr == nil {
		logger.Log().Debug().Int("interface_id", interfaceID).Msg("lsp.implementations: manager not configured")
		return nil
	}
	s.mu.Lock()
	// lazy eviction — remove expired entries on read
	if e, ok := s.implCache[interfaceID]; ok {
		if time.Since(e.at) < cacheTTL {
			s.mu.Unlock()
			return e.units
		}
		delete(s.implCache, interfaceID)
	}
	// cap cache size — evict oldest entry if at capacity
	if len(s.implCache) >= cacheMaxSize {
		var oldestKey int
		oldestAt := time.Now()
		for k, e := range s.implCache {
			if e.at.Before(oldestAt) {
				oldestAt = e.at
				oldestKey = k
			}
		}
		if oldestKey != 0 || len(s.implCache) > 0 {
			delete(s.implCache, oldestKey)
		}
	}
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	u, err := s.st.GetASTUnit(cctx, interfaceID)
	if err != nil || u == nil {
		logger.Log().Debug().Int("interface_id", interfaceID).Err(err).Msg("lsp.implementations: GetASTUnit failed")
		return nil
	}
	lang := fileutil.LanguageByExt(filepath.Ext(u.FilePath))
	if lang == "" {
		logger.Log().Debug().Str("file", u.FilePath).Msg("lsp.implementations: unknown language")
		return nil
	}
	cli, err := s.mgr.EnsureOpen(cctx, u.FilePath)
	if err != nil || cli == nil {
		logger.Log().Debug().Str("file", u.FilePath).Err(err).Msg("lsp.implementations: EnsureOpen failed")
		return nil
	}
	locs, err := cli.Implementation(cctx, u.FilePath, max0(u.StartLine-1), nameColumn(u.Signature, u.Name))
	if err != nil || len(locs) == 0 {
		logger.Log().Debug().Str("file", u.FilePath).Str("symbol", u.Name).
			Int("line", u.StartLine).Err(err).Int("locations", len(locs)).
			Msg("lsp.implementations: Implementation returned empty")
		return nil
	}
	units := s.locationsToUnits(cctx, locs, false /* exact unit at position */)
	s.mu.Lock()
	s.implCache[interfaceID] = cacheEntry{units: units, at: time.Now()}
	s.mu.Unlock()
	return units
}

// locationsToUnits сопоставляет LSP-локации с AST units из SQLite.
func (s *Service) locationsToUnits(ctx context.Context, locs []lsp.Location, enclosing bool) []store.ASTUnit {
	seen := map[int]struct{}{}
	out := make([]store.ASTUnit, 0, len(locs))
	for _, l := range locs {
		path := uriToPath(l.URI)
		if path == "" {
			continue
		}
		units, err := s.st.ListASTUnitsByFile(ctx, path)
		if err != nil || len(units) == 0 {
			continue
		}
		line := l.StartLine + 1 // store хранит 1-based
		var best *store.ASTUnit
		if enclosing {
			for i := range units {
				u := &units[i]
				if u.StartLine <= line && line <= u.EndLine && isFuncKind(u.Kind) {
					if best == nil || u.StartLine > best.StartLine {
						best = u
					}
				}
			}
		} else {
			for i := range units {
				u := &units[i]
				if u.StartLine == line || (u.StartLine <= line && line <= u.EndLine) {
					if best == nil || u.StartLine > best.StartLine {
						best = u
					}
				}
			}
		}
		if best == nil {
			continue
		}
		if _, ok := seen[best.ID]; ok {
			continue
		}
		seen[best.ID] = struct{}{}
		out = append(out, *best)
	}
	return out
}

func isFuncKind(kind string) bool {
	switch kind {
	case "function", "method", "constructor":
		return true
	}
	return false
}

func mergeUnits(a, b []store.ASTUnit) []store.ASTUnit {
	seen := make(map[int]struct{}, len(a)+len(b))
	out := make([]store.ASTUnit, 0, len(a)+len(b))
	for _, u := range a {
		if _, ok := seen[u.ID]; !ok {
			seen[u.ID] = struct{}{}
			out = append(out, u)
		}
	}
	for _, u := range b {
		if _, ok := seen[u.ID]; !ok {
			seen[u.ID] = struct{}{}
			out = append(out, u)
		}
	}
	return out
}

func uriToPath(uri string) string {
	const p = "file://"
	if len(uri) > len(p) && uri[:len(p)] == p {
		return uri[len(p):]
	}
	return uri
}

func max0(x int) int {
	if x < 0 {
		return 0
	}
	return x
}

// nameColumn эвристически вычисляет колонку (0-based) имени символа на строке
// определения. Если сигнатура содержит имя — берём индекс в сигнатуре, иначе 0.
func nameColumn(signature, name string) int {
	if name == "" {
		return 0
	}
	for i := 0; i+len(name) <= len(signature); i++ {
		if signature[i:i+len(name)] == name {
			return i
		}
	}
	return 0
}
