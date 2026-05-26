package store

// Файл реализует BFS-обход графа AST units по рёбрам:
// ExpandNeighbors (публичный API) + edgesAround (helper, симметричный
// обход in/out по рёбрам с фильтром по kinds).

import "context"

// ExpandNeighbors — BFS вокруг nodeID с глубиной depth. kinds — фильтр
// рёбер; пусто = все. Возвращает множества узлов и рёбер, без дубликатов.
// Обход и по исходящим, и по входящим рёбрам — для симметричного «contextual neighbourhood».
func (s *SQLite) ExpandNeighbors(ctx context.Context, nodeID int, depth int, kinds []string) ([]ASTUnit, []Edge, error) {
	if depth <= 0 {
		depth = 1
	}
	visited := map[int]bool{nodeID: true}
	seenEdges := map[int]bool{}
	frontier := []int{nodeID}
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
		var next []int
		for _, id := range frontier {
			outE, err := s.edgesAround(ctx, id, kinds)
			if err != nil {
				return nil, nil, err
			}
			for _, e := range outE {
				if e.ID != 0 {
					if seenEdges[e.ID] {
						continue
					}
					seenEdges[e.ID] = true
				}
				allEdges = append(allEdges, e)
				var other int
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

// TraverseGraph — направленный обход от startID по исходящим рёбрам.
func (s *SQLite) TraverseGraph(ctx context.Context, startID int, depth int, kinds []string) ([]ASTUnit, []Edge, error) {
	if depth <= 0 {
		depth = 1
	}
	visited := map[int]bool{startID: true}
	seenEdges := map[int]bool{}
	frontier := []int{startID}
	allNodes := []ASTUnit{}
	allEdges := []Edge{}

	root, err := s.GetASTUnit(ctx, startID)
	if err != nil {
		return nil, nil, err
	}
	if root != nil {
		allNodes = append(allNodes, *root)
	}

	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []int
		for _, id := range frontier {
			// Только исходящие ребра
			es, err := s.EdgesFrom(ctx, id, "")
			if err != nil {
				return nil, nil, err
			}

			for _, e := range es {
				// Фильтр по типам если задан
				if len(kinds) > 0 {
					found := false
					for _, k := range kinds {
						if e.Kind == k {
							found = true
							break
						}
					}
					if !found {
						continue
					}
				}

				if e.ID != 0 {
					if seenEdges[e.ID] {
						continue
					}
					seenEdges[e.ID] = true
				}
				allEdges = append(allEdges, e)
				if e.DstID == 0 || visited[e.DstID] {
					continue
				}

				visited[e.DstID] = true
				u, err := s.GetASTUnit(ctx, e.DstID)
				if err != nil {
					return nil, nil, err
				}
				if u != nil {
					allNodes = append(allNodes, *u)
					next = append(next, e.DstID)
				}
			}
		}
		frontier = next
	}
	return allNodes, allEdges, nil
}

func (s *SQLite) edgesAround(ctx context.Context, id int, kinds []string) ([]Edge, error) {
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
