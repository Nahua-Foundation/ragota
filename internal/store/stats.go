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

// AllFileHashes возвращает все file.hash (кроме служебных записей вроде __cross_hash__).
func (s *SQLite) AllFileHashes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hash FROM files WHERE path <> '__cross_hash__' AND hash <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}
	return hashes, rows.Err()
}

// HasCrossHashes проверяет, есть ли сохранённый cross_hash.
func (s *SQLite) HasCrossHashes(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE path = '__cross_hash__' AND hash <> ''`).Scan(&count)
	return count > 0, err
}
