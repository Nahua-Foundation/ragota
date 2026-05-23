package store

// Файл реализует BFS-обход графа AST units по рёбрам:
// ExpandNeighbors (публичный API) + edgesAround (helper, симметричный
// обход in/out по рёбрам с фильтром по kinds).

import "context"

// ExpandNeighbors — BFS вокруг nodeID с глубиной depth. kinds — фильтр
// рёбер; пусто = все. Возвращает множества узлов и рёбер, без дубликатов.
// Обход и по исходящим, и по входящим рёбрам — для симметричного «contextual neighbourhood».
func (s *SQLite) ExpandNeighbors(ctx context.Context, nodeID int64, depth int, kinds []string) ([]ASTUnit, []Edge, error) {
	if depth <= 0 {
		depth = 1
	}
	visited := map[int64]bool{nodeID: true}
	frontier := []int64{nodeID}
	allNodes := []ASTUnit{}
	allEdges := []Edge{}

	root, err := s.GetASTUnit(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}
	if root != nil {
		allNodes = append(allNodes, *root)
	}

	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []int64
		for _, id := range frontier {
			outE, err := s.edgesAround(ctx, id, kinds)
			if err != nil {
				return nil, nil, err
			}
			for _, e := range outE {
				allEdges = append(allEdges, e)
				var other int64
				if e.SrcID == id {
					other = e.DstID
				} else {
					other = e.SrcID
				}
				if other == 0 || visited[other] {
					continue
				}
				visited[other] = true
				u, err := s.GetASTUnit(ctx, other)
				if err != nil {
					return nil, nil, err
				}
				if u != nil {
					allNodes = append(allNodes, *u)
					next = append(next, other)
				}
			}
		}
		frontier = next
	}
	return allNodes, allEdges, nil
}

func (s *SQLite) edgesAround(ctx context.Context, id int64, kinds []string) ([]Edge, error) {
	q := `SELECT ` + edgeColumns + ` FROM edges WHERE (src_id = ? OR dst_id = ?)`
	args := []any{id, id}
	if len(kinds) > 0 {
		q += ` AND kind IN (`
		for i, k := range kinds {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, k)
		}
		q += `)`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanEdges(rows)
}
