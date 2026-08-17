package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// StoreFile stores a file metadata.
func (s *SQLite) StoreFile(ctx context.Context, f *storage.File) error {
	_, err := s.db.ExecContext(ctx, storeFileQuery, f.RepoID, f.Path, f.Language, f.Hash, f.Size, f.ModTime, f.Indexed)
	if err != nil {
		return fmt.Errorf("store file: %w", err)
	}
	return nil
}

// storeFileQuery is shared by the single-row and batch writers so the two
// cannot drift apart in what an upsert overwrites.
const storeFileQuery = `
	INSERT INTO files (repo_id, path, language, hash, size, mod_time, indexed)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (repo_id, path) DO UPDATE SET
		language = excluded.language,
		hash = excluded.hash,
		size = excluded.size,
		mod_time = excluded.mod_time,
		indexed = excluded.indexed
`

// BatchStoreFiles upserts many file rows in a single transaction.
func (s *SQLite) BatchStoreFiles(ctx context.Context, files []*storage.File) error {
	if len(files) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, storeFileQuery)
	if err != nil {
		return fmt.Errorf("prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, f := range files {
		if _, err := stmt.ExecContext(ctx,
			f.RepoID, f.Path, f.Language, f.Hash, f.Size, f.ModTime, f.Indexed,
		); err != nil {
			return fmt.Errorf("store file %s: %w", f.Path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// DeleteFilesByPaths deletes the given paths of one repo.
func (s *SQLite) DeleteFilesByPaths(ctx context.Context, repoID string, paths []string) error {
	return eachPathChunk(paths, func(batch []string) error {
		args := make([]any, 0, len(batch)+1)
		args = append(args, repoID)
		for _, p := range batch {
			args = append(args, p)
		}
		query := "DELETE FROM files WHERE repo_id = ? AND path IN (" + placeholders(len(batch)) + ")"
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("delete files by paths: %w", err)
		}
		return nil
	})
}

// GetFile gets a file by repo ID and path.
func (s *SQLite) GetFile(ctx context.Context, repoID, path string) (*storage.File, error) {

	query := `
		SELECT repo_id, path, language, hash, size, mod_time, indexed
		FROM files
		WHERE repo_id = ? AND path = ?
	`
	row := s.db.QueryRowContext(ctx, query, repoID, path)

	var f storage.File
	err := row.Scan(&f.RepoID, &f.Path, &f.Language, &f.Hash, &f.Size, &f.ModTime, &f.Indexed)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("get file: timeout")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}
	return &f, nil
}

// GetFilesByRepo gets all files for a repository.
func (s *SQLite) GetFilesByRepo(ctx context.Context, repoID string) ([]*storage.File, error) {

	query := `
		SELECT repo_id, path, language, hash, size, mod_time, indexed
		FROM files
		WHERE repo_id = ?
		ORDER BY path
	`
	rows, err := s.db.QueryContext(ctx, query, repoID)
	if err != nil {
		return nil, fmt.Errorf("query files: %w", err)
	}
	defer rows.Close()

	var files []*storage.File
	for rows.Next() {
		var f storage.File
		if err := rows.Scan(&f.RepoID, &f.Path, &f.Language, &f.Hash, &f.Size, &f.ModTime, &f.Indexed); err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}
		files = append(files, &f)
	}

	return files, rows.Err()
}

// DeleteFile deletes a file.
func (s *SQLite) DeleteFile(ctx context.Context, repoID, path string) error {

	_, err := s.db.ExecContext(ctx, "DELETE FROM files WHERE repo_id = ? AND path = ?", repoID, path)
	if err != nil {
		return fmt.Errorf("delete file: %w", err)
	}
	return nil
}

// DeleteFilesByRepo deletes all files for a repository.
func (s *SQLite) DeleteFilesByRepo(ctx context.Context, repoID string) error {

	_, err := s.db.ExecContext(ctx, "DELETE FROM files WHERE repo_id = ?", repoID)
	if err != nil {
		return fmt.Errorf("delete files by repo: %w", err)
	}
	return nil
}

// GetFilesByHash gets files with a specific hash (for reindex checking).
func (s *SQLite) GetFilesByHash(ctx context.Context, repoID, hash string) ([]*storage.File, error) {

	query := `
		SELECT repo_id, path, language, hash, size, mod_time, indexed
		FROM files
		WHERE repo_id = ? AND hash = ?
	`
	rows, err := s.db.QueryContext(ctx, query, repoID, hash)
	if err != nil {
		return nil, fmt.Errorf("query files by hash: %w", err)
	}
	defer rows.Close()

	var files []*storage.File
	for rows.Next() {
		var f storage.File
		if err := rows.Scan(&f.RepoID, &f.Path, &f.Language, &f.Hash, &f.Size, &f.ModTime, &f.Indexed); err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}
		files = append(files, &f)
	}

	return files, rows.Err()
}

// MarkFilesIndexed marks files as indexed.
func (s *SQLite) MarkFilesIndexed(ctx context.Context, repoID string, paths []string) error {

	if len(paths) == 0 {
		return nil
	}

	query := "UPDATE files SET indexed = 1 WHERE repo_id = ? AND path IN ("
	args := []interface{}{repoID}
	for i := range paths {
		if i > 0 {
			query += ","
		}
		query += "?"
		args = append(args, paths[i])
	}
	query += ")"

	_, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("mark files indexed: %w", err)
	}
	return nil
}
