package graph

import (
	"context"
	"path/filepath"
	"time"

	"ragota/pkg/fileutil"
	"ragota/pkg/lsp"
	"ragota/internal/store"
)

// lspCallers выполняет textDocument/references на позиции определения функции
// и резолвит локации обратно в AST units. При любой ошибке возвращает nil
// (fallback на tree-sitter обеспечивается вызывающим).
func (s *Service) lspCallers(ctx context.Context, unitID int) []store.ASTUnit {
	if s.mgr == nil {
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
		return nil
	}
	lang := fileutil.LanguageByExt(filepath.Ext(u.FilePath))
	if lang == "" {
		return nil
	}
	cli, err := s.mgr.EnsureOpen(cctx, u.FilePath)
	if err != nil || cli == nil {
		return nil
	}
	locs, err := cli.References(cctx, u.FilePath, max0(u.StartLine-1), nameColumn(u.Signature, u.Name), false)
	if err != nil || len(locs) == 0 {
		return nil
	}
	units := s.locationsToUnits(cctx, locs, true /* enclosing */)
	s.mu.Lock()
	s.callCache[unitID] = cacheEntry{units: units, at: time.Now()}
	s.mu.Unlock()
	return units
}

// lspImplementations выполняет textDocument/implementation для интерфейса и
// возвращает AST units реализаций.
func (s *Service) lspImplementations(ctx context.Context, interfaceID int) []store.ASTUnit {
	if s.mgr == nil {
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
		return nil
	}
	lang := fileutil.LanguageByExt(filepath.Ext(u.FilePath))
	if lang == "" {
		return nil
	}
	cli, err := s.mgr.EnsureOpen(cctx, u.FilePath)
	if err != nil || cli == nil {
		return nil
	}
	locs, err := cli.Implementation(cctx, u.FilePath, max0(u.StartLine-1), nameColumn(u.Signature, u.Name))
	if err != nil || len(locs) == 0 {
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
