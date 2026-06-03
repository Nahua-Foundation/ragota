package symbols

import (
	"context"
	"os"
	"sort"
	"strings"

	"ragota/internal/search/graph"
	"ragota/internal/store"
)

// SurroundingContext возвращает текст вокруг unit: его body + beforeLines/afterLines.
func (s *Service) SurroundingContext(ctx context.Context, id int, beforeLines, afterLines int) (string, error) {
	u, err := s.st.GetASTUnit(ctx, id)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", ErrNotFound
	}
	if u.FilePath == "" {
		return "", nil
	}
	src, err := os.ReadFile(u.FilePath)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(src), "\n")
	start := u.StartLine - 1 - beforeLines
	end := u.EndLine + afterLines
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n"), nil
}

// RelatedFiles — файлы, связанные с символом через import/call/reference.
func (s *Service) RelatedFiles(ctx context.Context, id int) ([]string, error) {
	nb, err := s.g.ExpandNeighbors(ctx, id, 1, []string{graph.EdgeCall, graph.EdgeImport, graph.EdgeReference, graph.EdgeImplements, graph.EdgeExtends})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, n := range nb.Nodes {
		if n.FilePath != "" {
			seen[n.FilePath] = true
		}
	}
	for _, e := range nb.Edges {
		if e.FilePath != "" {
			seen[e.FilePath] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// SimilarCode — делегирует SimilarSearcher (vector), если он подключён.
func (s *Service) SimilarCode(ctx context.Context, id int, limit int) ([]store.ASTUnit, error) {
	if s.sim == nil {
		return []store.ASTUnit{}, nil
	}
	u, err := s.st.GetASTUnit(ctx, id)
	if err != nil || u == nil {
		return nil, err
	}
	return s.sim.SimilarToUnit(ctx, *u, limit)
}
