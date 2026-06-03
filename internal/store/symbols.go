package store

import (
	"context"
)

// SymbolRow — строка таблицы symbols.
type SymbolRow struct {
	ID         int64
	FilePath   string
	Name       string
	Kind       string // function/method/class/struct/interface/var/const/...
	StartLine  int
	EndLine    int
	StartByte  int
	EndByte    int
	ParentName string
	Signature  string
}

// SearchSymbols ищет символы по подстроке имени, опционально фильтруя по kind и языку.
func (s *SQLite) SearchSymbols(ctx context.Context, query, kind, language string, limit int) ([]SymbolRow, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT s.id, s.file_path, s.name, s.kind, s.start_line, s.end_line, s.start_byte, s.end_byte, s.parent_name, s.signature
	      FROM symbols s JOIN files f ON f.path = s.file_path
	      WHERE s.name LIKE ?`
	args := []any{"%" + query + "%"}
	if kind != "" {
		q += ` AND s.kind = ?`
		args = append(args, kind)
	}
	if language != "" {
		q += ` AND f.language = ?`
		args = append(args, language)
	}
	q += ` ORDER BY length(s.name) ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SymbolRow{}
	for rows.Next() {
		var r SymbolRow
		if err := rows.Scan(&r.ID, &r.FilePath, &r.Name, &r.Kind, &r.StartLine, &r.EndLine, &r.StartByte, &r.EndByte, &r.ParentName, &r.Signature); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SymbolsByFile возвращает все символы из файла, упорядоченные по позиции.
func (s *SQLite) SymbolsByFile(ctx context.Context, path string) ([]SymbolRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, file_path, name, kind, start_line, end_line, start_byte, end_byte, parent_name, signature
		 FROM symbols WHERE file_path = ? ORDER BY start_byte`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SymbolRow{}
	for rows.Next() {
		var r SymbolRow
		if err := rows.Scan(&r.ID, &r.FilePath, &r.Name, &r.Kind, &r.StartLine, &r.EndLine, &r.StartByte, &r.EndByte, &r.ParentName, &r.Signature); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
