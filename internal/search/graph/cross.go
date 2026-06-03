// Package graph — code-graph (calls/imports/implementations/extends/references)
// поверх таблиц store.ast_units и store.edges.
//
// Этот файл — cross-repo расширение: сервисный граф, cross-repo callers,
// resolve cross-repo вызовов.
package graph

import (
	"context"
	"ragota/internal/store"
)

// ServiceGraph возвращает граф зависимостей между репозиториями.
// Ключи — имена репо, значения — сводка зависимостей.
func (s *Service) ServiceGraph(ctx context.Context) map[string]*ServiceNode {
	nodes := make(map[string]*ServiceNode)

	// Получаем все edges с dst_repo != "" (cross-repo)
	allEdges, err := s.st.AllCrossRepoEdges(ctx)
	if err != nil {
		return nodes
	}

	for _, e := range allEdges {
		// Source node
		if _, ok := nodes[e.Repo]; !ok {
			nodes[e.Repo] = &ServiceNode{Name: e.Repo}
		}
		src := nodes[e.Repo]

		// Target node
		if _, ok := nodes[e.DstRepo]; !ok {
			nodes[e.DstRepo] = &ServiceNode{Name: e.DstRepo}
		}
		dst := nodes[e.DstRepo]

		// Add dependency
		src.Dependencies = append(src.Dependencies, DepEdge{
			Target:   e.DstRepo,
			Protocol: e.Kind,
			Count:    1,
		})
		dst.DependedBy = append(dst.DependedBy, e.Repo)
	}

	// Aggregate
	for _, node := range nodes {
		node.Dependencies = aggregateDeps(node.Dependencies)
		node.DependedBy = uniqueStrings(node.DependedBy)
	}

	return nodes
}

// ServiceNode — узел сервисного графа.
type ServiceNode struct {
	Name         string     `json:"name"`
	Dependencies []DepEdge  `json:"dependencies"`
	DependedBy   []string   `json:"depended_by"`
}

// DepEdge — ребро зависимости.
type DepEdge struct {
	Target   string `json:"target"`
	Protocol string `json:"protocol"`
	Count    int    `json:"count"`
}

// CrossRepoCallers возвращает callers символа из всех репозиториев.
// В отличие от Callers, не фильтрует по repo — ищет по всем edges с dst_name = symbol.
func (s *Service) CrossRepoCallers(ctx context.Context, symbolName string) ([]store.Edge, error) {
	return s.st.EdgesByDstName(ctx, symbolName, "call")
}

// ResolveCrossCall находит, куда ведёт cross-repo вызов.
// Возвращает edge с dst_repo и (если найдено) target AST unit.
func (s *Service) ResolveCrossCall(ctx context.Context, filePath string, line int) (*CrossCallResolution, error) {
	// Находим unit по file + line
	units, err := s.st.FindASTUnitsByFileLine(ctx, filePath, line)
	if err != nil {
		return nil, err
	}
	if len(units) == 0 {
		return nil, nil
	}

	unit := units[0]
	edges, err := s.st.EdgesFrom(ctx, unit.ID, "")
	if err != nil {
		return nil, err
	}

	for _, e := range edges {
		if e.DstRepo != "" || (e.DstID == 0 && e.DstName != "") {
			// Cross-repo edge
			res := &CrossCallResolution{
				Edge:      e,
				SrcSymbol: unit,
			}

			// Попытаемся найти target unit в dst_repo
			if e.DstRepo != "" {
				targets, _ := s.st.FindASTUnits(ctx, e.DstName, "", "", e.DstRepo, 10)
				if len(targets) > 0 {
					res.DstSymbol = &targets[0]
				}
			}

			return res, nil
		}
	}

	return nil, nil
}

// CrossCallResolution — результат resolve cross-repo вызова.
type CrossCallResolution struct {
	Edge      store.Edge      `json:"edge"`
	SrcSymbol store.ASTUnit   `json:"src_symbol"`
	DstSymbol *store.ASTUnit  `json:"dst_symbol,omitempty"`
}

func aggregateDeps(deps []DepEdge) []DepEdge {
	agg := make(map[string]*DepEdge)
	for _, d := range deps {
		key := d.Target + "|" + d.Protocol
		if existing, ok := agg[key]; ok {
			existing.Count++
		} else {
			copy := d
			agg[key] = &copy
		}
	}
	out := make([]DepEdge, 0, len(agg))
	for _, d := range agg {
		out = append(out, *d)
	}
	return out
}

func uniqueStrings(ss []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
