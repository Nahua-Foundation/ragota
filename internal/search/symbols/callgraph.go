package symbols

import (
	"context"
	"strings"

	"ragota/internal/search/graph"
	"ragota/internal/store"
)

// FindCallers возвращает функции/методы, вызывающие данную функцию.
func (s *Service) FindCallers(ctx context.Context, function string) ([]store.ASTUnit, error) {
	defs, err := s.findCallable(ctx, function)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	out := []store.ASTUnit{}

	for _, d := range defs {
		cs, err := s.g.Callers(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			out = append(out, c)
		}
	}

	langs := map[string]bool{}
	for _, d := range defs {
		if d.Language != "" {
			langs[d.Language] = true
		}
	}

	byNameAll := s.edgesByName(ctx, function, graph.EdgeCall, langs)

	if parts := strings.Split(function, "."); len(parts) > 1 {
		lastName := parts[len(parts)-1]
		byLastName := s.edgesByName(ctx, lastName, graph.EdgeCall, langs)
		byNameAll = append(byNameAll, byLastName...)
	}
	for _, e := range byNameAll {
		if seen[e.SrcID] {
			continue
		}
		u, err := s.st.GetASTUnit(ctx, e.SrcID)
		if err != nil || u == nil {
			continue
		}
		seen[u.ID] = true
		out = append(out, *u)
	}

	return out, nil
}

// FindCallees возвращает функции/методы, которые вызывает данная функция.
func (s *Service) FindCallees(ctx context.Context, function string) ([]store.ASTUnit, error) {
	defs, err := s.findCallable(ctx, function)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	out := []store.ASTUnit{}
	for _, d := range defs {
		cs, err := s.g.Callees(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			out = append(out, c)
		}

		// Fallback: ищем цели по имени из unresolved edges.
		edges, _ := s.st.EdgesFrom(ctx, d.ID, graph.EdgeCall)
		for _, e := range edges {
			if e.DstID == 0 && e.DstName != "" {
				names := []string{e.DstName}
				if dot := strings.LastIndex(e.DstName, "."); dot >= 0 {
					names = append(names, e.DstName[dot+1:])
				}
				for _, name := range names {
					targets, _ := s.st.FindASTUnits(ctx, name, "function", d.Language, "", 50)
					meths, _ := s.st.FindASTUnits(ctx, name, "method", d.Language, "", 50)
					targets = append(targets, meths...)
					for _, t := range targets {
						if !seen[t.ID] {
							seen[t.ID] = true
							out = append(out, t)
						}
					}
				}
			}
		}
	}
	return out, nil
}

// edgesByName ищет edges по dst_name с фильтром по языкам.
func (s *Service) edgesByName(ctx context.Context, name, kind string, langs map[string]bool) []store.Edge {
	if len(langs) == 0 {
		edges, _ := s.st.EdgesByDstName(ctx, name, kind)
		return edges
	}
	var all []store.Edge
	for lang := range langs {
		part, _ := s.st.EdgesByDstNameForLang(ctx, name, kind, lang)
		all = append(all, part...)
	}
	return all
}
