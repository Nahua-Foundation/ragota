package symbols

import (
	"context"
	"strings"

	"ragota/internal/search/graph"
	"ragota/internal/store"
)

// FindImplementations возвращает реализации интерфейса.
func (s *Service) FindImplementations(ctx context.Context, iface string) ([]store.ASTUnit, error) {
	var units []store.ASTUnit
	for _, k := range []string{"interface", "class", "type", "struct"} {
		us, err := s.st.FindASTUnits(ctx, iface, k, "", "", 50)
		if err == nil {
			units = append(units, us...)
		}
	}
	if len(units) == 0 {
		return nil, nil
	}
	out := []store.ASTUnit{}
	seen := map[int]bool{}

	// 0. LSP для точного поиска реализаций.
	if s.mgr != nil {
		s.addLSPImplementations(ctx, units, seen, &out)
	}

	// 1. По ID найденных интерфейсов.
	for _, u := range units {
		impls, err := s.g.Implementations(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		for _, im := range impls {
			if seen[im.ID] {
				continue
			}
			seen[im.ID] = true
			out = append(out, im)
		}

		// Эвристика для Go/TS/JS: методы интерфейса → структуры/классы с теми же методами.
		if u.Language == "go" || u.Language == "typescript" || u.Language == "javascript" {
			s.findMethodMatchImplementations(ctx, u, seen, &out)
		}
	}

	// 2. По имени (для неразрешённых связей).
	langs := map[string]bool{}
	for _, u := range units {
		if u.Language != "" {
			langs[u.Language] = true
		}
	}
	var byNameAll []store.Edge
	if len(langs) == 0 {
		var err error
		byNameAll, err = s.st.EdgesByDstName(ctx, iface, graph.EdgeImplements)
		if err != nil {
			return nil, err
		}
	} else {
		for lang := range langs {
			part, err := s.st.EdgesByDstNameForLang(ctx, iface, graph.EdgeImplements, lang)
			if err != nil {
				return nil, err
			}
			byNameAll = append(byNameAll, part...)
		}
	}
	for _, e := range byNameAll {
		if seen[e.SrcID] {
			continue
		}
		u, err := s.st.GetASTUnit(ctx, e.SrcID)
		if err != nil || u == nil {
			continue
		}
		if u.Language == "go" && u.Kind == "interface" {
			continue
		}
		seen[u.ID] = true
		out = append(out, *u)
	}

	return out, nil
}

func (s *Service) addLSPImplementations(ctx context.Context, units []store.ASTUnit, seen map[int]bool, out *[]store.ASTUnit) {
	for _, u := range units {
		c, err := s.mgr.EnsureOpen(ctx, u.FilePath)
		if err != nil || c == nil {
			continue
		}
		line := u.StartLine - 1
		col := 0
		if u.NameStartLine > 0 {
			line = u.NameStartLine - 1
			col = u.NameStartCol
		}
		locs, err := c.Implementation(ctx, u.FilePath, line, col)
		if err != nil || len(locs) == 0 {
			continue
		}
		for _, loc := range locs {
			path := strings.TrimPrefix(loc.URI, "file://")
			fileUnits, _ := s.st.ListASTUnitsByFile(ctx, path)
			for _, fu := range fileUnits {
				if fu.StartLine == loc.StartLine+1 || fu.NameStartLine == loc.StartLine+1 {
					if !seen[fu.ID] {
						seen[fu.ID] = true
						*out = append(*out, fu)
					}
				}
			}
		}
	}
}

func (s *Service) findMethodMatchImplementations(ctx context.Context, u store.ASTUnit, seen map[int]bool, out *[]store.ASTUnit) {
	methods, err := s.st.ChildrenOf(ctx, u.ID)
	if err != nil || len(methods) == 0 {
		return
	}
	methodNames := make([]string, 0, len(methods))
	for _, m := range methods {
		if m.Kind == "method" || m.Kind == "function" {
			methodNames = append(methodNames, m.Name)
		}
	}
	if len(methodNames) == 0 {
		return
	}

	maxMethodsToCheck := 3
	if len(methodNames) < maxMethodsToCheck {
		maxMethodsToCheck = len(methodNames)
	}

	for i := 0; i < maxMethodsToCheck; i++ {
		candidates, err := s.st.FindASTUnits(ctx, methodNames[i], "method", u.Language, "", 100)
		if err != nil {
			continue
		}
		for _, cand := range candidates {
			if !cand.ParentID.Valid {
				continue
			}
			owner, err := s.st.GetASTUnit(ctx, int(cand.ParentID.Int64))
			if err != nil || owner == nil {
				continue
			}
			isPotentialOwner := false
			if u.Language == "go" {
				isPotentialOwner = (owner.Kind == "struct" || owner.Kind == "type")
			} else {
				isPotentialOwner = (owner.Kind == "class" || owner.Kind == "interface")
			}
			if !isPotentialOwner || seen[owner.ID] {
				continue
			}
			ownerMethods, err := s.st.ChildrenOf(ctx, owner.ID)
			if err != nil {
				continue
			}
			ownerMethodNames := make(map[string]bool)
			for _, om := range ownerMethods {
				ownerMethodNames[om.Name] = true
			}
			matchCount := 0
			for _, mn := range methodNames {
				if ownerMethodNames[mn] {
					matchCount++
				}
			}
			if matchCount == len(methodNames) {
				seen[owner.ID] = true
				*out = append(*out, *owner)
			}
		}
	}
}
