package rerank

// Файл содержит noop-реализацию Reranker: используется, когда реранкер
// не сконфигурирован (пустой URL/Model) либо когда настоящая модель
// недоступна и Required=false — возвращается исходный порядок кандидатов.

import "context"

type noop struct{ opts Options }

func (n *noop) Available(ctx context.Context) bool { return false }
func (n *noop) SetSemaphore(sem chan struct{})     {}

func (n *noop) Rerank(ctx context.Context, query string, candidates []Candidate, topN int) ([]Scored, error) {
	if n.opts.Required {
		return nil, ErrUnavailable
	}
	return identity(candidates, topN), nil
}
