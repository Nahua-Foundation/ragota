package symbols

import (
	"context"
	"strings"

	"ragota/internal/search/graph"
	"ragota/internal/store"
)

// FindReferences возвращает все рёбра, ссылающиеся на символ.
func (s *Service) FindReferences(ctx context.Context, symbol string) ([]store.Edge, error) {
	defs, err := s.FindDefinition(ctx, symbol)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var out []store.Edge = []store.Edge{}

	type nameLangKey struct {
		name string
		lang string
	}
	namesToSearch := map[nameLangKey]bool{}
	for _, d := range defs {
		es, err := s.g.References(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			out = append(out, e)
		}
		namesToSearch[nameLangKey{d.Name, d.Language}] = true
		if d.Qualified != "" {
			namesToSearch[nameLangKey{d.Qualified, d.Language}] = true
		}
	}

	kinds := []string{graph.EdgeReference, graph.EdgeImplements, graph.EdgeExtends, graph.EdgeCall}
	if len(namesToSearch) == 0 {
		for _, kind := range kinds {
			byName, err := s.st.EdgesByDstName(ctx, symbol, kind)
			if err != nil {
				continue
			}
			for _, e := range byName {
				if seen[e.ID] {
					continue
				}
				seen[e.ID] = true
				out = append(out, e)
			}
		}

		if parts := strings.Split(symbol, "."); len(parts) > 1 {
			lastName := parts[len(parts)-1]
			for _, kind := range kinds {
				byLastName, err := s.st.EdgesByDstName(ctx, lastName, kind)
				if err == nil {
					for _, e := range byLastName {
						if seen[e.ID] {
							continue
						}
						seen[e.ID] = true
						out = append(out, e)
					}
				}
			}
		}

		return out, nil
	}

	for k := range namesToSearch {
		for _, kind := range kinds {
			byName, err := s.st.EdgesByDstNameForLang(ctx, k.name, kind, k.lang)
			if err != nil {
				return nil, err
			}
			for _, e := range byName {
				if seen[e.ID] {
					continue
				}
				seen[e.ID] = true
				out = append(out, e)
			}
		}
	}
	return out, nil
}
