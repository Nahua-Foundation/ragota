package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"time"
)

// FileRow — строка таблицы files.
type FileRow struct {
	Path      string
	Language  string
	Hash      string
	Size      int64
	ModTime   time.Time
	IndexedAt time.Time
	Symbols   int
	VecHash   string
	CrossHash string
}

// GetFile возвращает строку файла или nil, если не найдена.
func (s *SQLite) GetFile(ctx context.Context, path string) (*FileRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT path, language, hash, size, mod_time, indexed_at, symbol_cnt, vec_hash, cross_hash FROM files WHERE path = ?`,
		path)
	var fr FileRow
	var modTime, indexedAt int64
	err := row.Scan(&fr.Path, &fr.Language, &fr.Hash, &fr.Size, &modTime, &indexedAt, &fr.Symbols, &fr.VecHash, &fr.CrossHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fr.ModTime = time.Unix(modTime, 0)
	fr.IndexedAt = time.Unix(indexedAt, 0)
	return &fr, nil
}

// UpsertFile добавляет/обновляет запись файла и заменяет его символы атомарно.
func (s *SQLite) UpsertFile(ctx context.Context, f FileRow, symbols []SymbolRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO files(path, language, hash, size, mod_time, indexed_at, symbol_cnt, vec_hash)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET
		   language=excluded.language,
		   hash=excluded.hash,
		   size=excluded.size,
		   mod_time=excluded.mod_time,
		   indexed_at=excluded.indexed_at,
		   symbol_cnt=excluded.symbol_cnt,
		   vec_hash=CASE WHEN excluded.vec_hash != '' THEN excluded.vec_hash ELSE files.vec_hash END`,
		f.Path, f.Language, f.Hash, f.Size, f.ModTime.Unix(), time.Now().Unix(), len(symbols), f.VecHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM symbols WHERE file_path = ?`, f.Path); err != nil {
		return err
	}
	if len(symbols) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO symbols(file_path, name, kind, start_line, end_line, start_byte, end_byte, parent_name, signature)
			 VALUES(?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, sym := range symbols {
			if _, err := stmt.ExecContext(ctx,
				f.Path, sym.Name, sym.Kind, sym.StartLine, sym.EndLine,
				sym.StartByte, sym.EndByte, sym.ParentName, sym.Signature); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// EnsureFile гарантирует наличие записи файла в таблице files.
func (s *SQLite) EnsureFile(ctx context.Context, path, lang string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO files(path, language, hash, size, mod_time, indexed_at, symbol_cnt, vec_hash)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(path) DO NOTHING`,
		path, lang, "", 0, 0, time.Now().Unix(), 0, "")
	return err
}

// UpdateVectorHash обновляет только хэш векторного индекса.
func (s *SQLite) UpdateVectorHash(ctx context.Context, path, hash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE files SET vec_hash = ? WHERE path = ?`, hash, path)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO files(path, language, hash, size, mod_time, indexed_at, symbol_cnt, vec_hash)
			 VALUES(?,?,?,?,?,?,?,?)
			 ON CONFLICT(path) DO UPDATE SET vec_hash=excluded.vec_hash`,
			path, "", "", 0, 0, time.Now().Unix(), 0, hash)
	}
	return err
}

// GetFileHash возвращает хэш содержимого файла из таблицы files.
func (s *SQLite) GetFileHash(ctx context.Context, path string) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT hash FROM files WHERE path = ?`, path).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hash, err
}

// UpdateFileHash обновляет хэш содержимого файла в таблице files.
func (s *SQLite) UpdateFileHash(ctx context.Context, path, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET hash = ? WHERE path = ?`, hash, path)
	if err != nil {
		return err
	}
	lang := ""
	if ext := filepath.Ext(path); ext != "" {
		lang = ext[1:]
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO files(path, language, hash, size, mod_time, indexed_at, symbol_cnt, vec_hash)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET hash=excluded.hash`,
		path, lang, hash, 0, 0, time.Now().Unix(), 0, "")
	return err
}

// UpsertVectorHash — специальный апсерт для векторного индекса.
func (s *SQLite) UpsertVectorHash(ctx context.Context, path, hash, lang string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO files(path, language, hash, size, mod_time, indexed_at, symbol_cnt, vec_hash)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET
		   vec_hash=excluded.vec_hash,
		   language=excluded.language,
		   size=excluded.size,
		   mod_time=excluded.mod_time`,
		path, lang, "", info.Size(), info.ModTime().Unix(), time.Now().Unix(), 0, hash)
	return err
}

// DeleteFile удаляет файл и его символы.
func (s *SQLite) DeleteFile(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE path = ?`, path)
	return err
}

// ResetVecHashes сбрасывает все vec_hash в таблице files.
func (s *SQLite) ResetVecHashes(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET vec_hash = '' WHERE vec_hash != ''`)
	return err
}

// HasFileHashes проверяет, есть ли в таблице files записи с hash.
func (s *SQLite) HasFileHashes(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE hash != ''`).Scan(&count)
	return count > 0, err
}

// ResetFileHashes сбрасывает все hash в таблице files.
func (s *SQLite) ResetFileHashes(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET hash = '' WHERE hash != ''`)
	return err
}

// HasVecHashes проверяет, есть ли в таблице files записи с vec_hash.
func (s *SQLite) HasVecHashes(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE vec_hash != ''`).Scan(&count)
	return count > 0, err
}

// GetCrossHash возвращает агрегированный хеш crossrepo-состояния.
// Хранится в одной записи с path = '__cross_hash__'.
func (s *SQLite) GetCrossHash(ctx context.Context) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT hash FROM files WHERE path = '__cross_hash__'`).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hash, err
}

// SetCrossHash сохраняет агрегированный хеш crossrepo-состояния.
func (s *SQLite) SetCrossHash(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO files(path, language, hash, size, mod_time, indexed_at, symbol_cnt, vec_hash, cross_hash)
		 VALUES('__cross_hash__','','','',0,0,0,'','')
		 ON CONFLICT(path) DO UPDATE SET hash=excluded.hash`,
		hash)
	return err
}

// ResetCrossHashes сбрасывает cross_hash (для обнаружения stale состояния).
func (s *SQLite) ResetCrossHashes(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET cross_hash = '' WHERE cross_hash != ''`)
	return err
}

// DeleteCrossCallEdges удаляет все edges вида cross_call.
func (s *SQLite) DeleteCrossCallEdges(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM edges WHERE kind = 'cross_call'`)
	return err
}
