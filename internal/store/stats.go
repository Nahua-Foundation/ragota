package store

import "context"

// Stats — общая статистика индекса.
type Stats struct {
	Files   int
	Symbols int
}

// GraphStats — статистика графового индекса.
type GraphStats struct {
	Units int
	Edges int
}

// Stats возвращает количество файлов и символов.
func (s *SQLite) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&st.Files); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbols`).Scan(&st.Symbols); err != nil {
		return st, err
	}
	return st, nil
}

// GraphStats возвращает количество AST юнитов и ребер.
func (s *SQLite) GraphStats(ctx context.Context) (GraphStats, error) {
	var st GraphStats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ast_units`).Scan(&st.Units); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges`).Scan(&st.Edges); err != nil {
		return st, err
	}
	return st, nil
}
