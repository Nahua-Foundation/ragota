package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Nahua-Foundation/ragota/internal/store"
)

// StoreFile stores a file metadata row, upserting by (repo_id, path).
func (s *SQLite) StoreFile(ctx context.Context, f *store.File) error {
	if err := s.q.StoreFile(ctx, StoreFileParams{
		RepoID: f.RepoID, Path: f.Path, Language: f.Language, Hash: f.Hash,
		Size: f.Size, ModTime: f.ModTime, Indexed: b2i(f.Indexed),
	}); err != nil {
		return fmt.Errorf("store file: %w", err)
	}
	return nil
}

// BatchStoreFiles upserts many file rows in a single transaction. An index
// pass writes one row per file, and one autocommit transaction per row is a
// per-file WAL commit that dominates the bookkeeping cost on a repository with
// tens of thousands of files.
func (s *SQLite) BatchStoreFiles(ctx context.Context, files []*store.File) error {
	if len(files) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, storeFile)
	if err != nil {
		return fmt.Errorf("prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, f := range files {
		if _, err := stmt.ExecContext(ctx,
			f.RepoID, f.Path, f.Language, f.Hash, f.Size, f.ModTime, b2i(f.Indexed),
		); err != nil {
			return fmt.Errorf("store file %s: %w", f.Path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// DeleteFilesByPaths deletes the given paths of one repo in one statement per
// chunk, rather than one round-trip per path.
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
func (s *SQLite) GetFile(ctx context.Context, repoID, path string) (*store.File, error) {
	r, err := s.q.GetFile(ctx, GetFileParams{RepoID: repoID, Path: path})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}
	return fileFromRow(r), nil
}

// GetFilesByRepo gets all files for a repository.
func (s *SQLite) GetFilesByRepo(ctx context.Context, repoID string) ([]*store.File, error) {
	rows, err := s.q.GetFilesByRepo(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("query files: %w", err)
	}
	files := make([]*store.File, 0, len(rows))
	for _, r := range rows {
		files = append(files, fileFromRow(r))
	}
	return files, nil
}

// DeleteFile deletes a file.
func (s *SQLite) DeleteFile(ctx context.Context, repoID, path string) error {
	if err := s.q.DeleteFile(ctx, DeleteFileParams{RepoID: repoID, Path: path}); err != nil {
		return fmt.Errorf("delete file: %w", err)
	}
	return nil
}

// DeleteFilesByRepo deletes all files for a repository.
func (s *SQLite) DeleteFilesByRepo(ctx context.Context, repoID string) error {
	if err := s.q.DeleteFilesByRepo(ctx, repoID); err != nil {
		return fmt.Errorf("delete files by repo: %w", err)
	}
	return nil
}

// GetFilesByHash gets files with a specific hash (for reindex checking).
func (s *SQLite) GetFilesByHash(ctx context.Context, repoID, hash string) ([]*store.File, error) {
	rows, err := s.q.GetFilesByHash(ctx, GetFilesByHashParams{RepoID: repoID, Hash: hash})
	if err != nil {
		return nil, fmt.Errorf("query files by hash: %w", err)
	}
	files := make([]*store.File, 0, len(rows))
	for _, r := range rows {
		files = append(files, fileFromRow(r))
	}
	return files, nil
}

// MarkFilesIndexed marks files as indexed.
func (s *SQLite) MarkFilesIndexed(ctx context.Context, repoID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	return eachPathChunk(paths, func(batch []string) error {
		args := make([]any, 0, len(batch)+1)
		args = append(args, repoID)
		for _, p := range batch {
			args = append(args, p)
		}
		query := "UPDATE files SET indexed = 1 WHERE repo_id = ? AND path IN (" + placeholders(len(batch)) + ")"
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("mark files indexed: %w", err)
		}
		return nil
	})
}
