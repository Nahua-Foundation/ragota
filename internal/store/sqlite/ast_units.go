package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

func scanASTUnit(sc scanner) (*domain.ASTUnit, error) {
	var u domain.ASTUnit
	var parentID sql.NullInt64
	err := sc.Scan(
		&u.ID, &u.RepoID, &u.FilePath, &u.Language, &u.Kind, &u.Name, &u.Qualified,
		&parentID, &u.StartLine, &u.EndLine, &u.StartByte, &u.EndByte,
		&u.Signature, &u.Doc, &u.Hash, &u.Meta,
	)
	if err != nil {
		return nil, err
	}
	if parentID.Valid {
		u.ParentID = fmt.Sprintf("%d", parentID.Int64)
	}
	return &u, nil
}

// StoreASTUnit stores an AST unit, assigning its new ID back into u.
func (s *SQLite) StoreASTUnit(ctx context.Context, u *domain.ASTUnit) error {
	id, err := s.q.InsertASTUnit(ctx, unitInsertParams(u))
	if err != nil {
		return fmt.Errorf("store ast unit: %w", err)
	}
	if u.ID == "" {
		u.ID = fmt.Sprintf("%d", id)
	}
	return nil
}

// GetASTUnits gets AST units with optional filtering. The filtering, tiering
// and ranking are storage's, shared with the Postgres backend; only running the
// statement is this backend's own.
func (s *SQLite) GetASTUnits(ctx context.Context, opts domain.QueryOpts) ([]*domain.ASTUnit, error) {
	return store.QueryUnits(store.SQLiteDialect{}, opts,
		func(id string) any { return intOrZero(id) },
		func(query string, args []any) ([]*domain.ASTUnit, error) {
			rows, err := s.db.QueryContext(ctx, query, args...)
			if err != nil {
				return nil, fmt.Errorf("query ast units: %w", err)
			}
			defer rows.Close()

			var units []*domain.ASTUnit
			for rows.Next() {
				u, err := scanASTUnit(rows)
				if err != nil {
					return nil, fmt.Errorf("scan ast unit: %w", err)
				}
				units = append(units, u)
			}

			return units, rows.Err()
		})
}

// DeleteASTUnitsByFile deletes a file's units and unresolves the edges that
// pointed at them, in one transaction.
func (s *SQLite) DeleteASTUnitsByFile(ctx context.Context, repoID, filePath string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete ast units by file: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE edges SET dst_id = 0, dst_repo_id = ''
		 WHERE dst_id <> 0
		   AND dst_id IN (SELECT id FROM ast_units WHERE repo_id = ? AND file_path = ?)`,
		repoID, filePath,
	); err != nil {
		return fmt.Errorf("unresolve edges into file: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM ast_units WHERE repo_id = ? AND file_path = ?", repoID, filePath,
	); err != nil {
		return fmt.Errorf("delete ast units by file: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete ast units by file: %w", err)
	}
	return nil
}

// DeleteASTUnitsByFiles is DeleteASTUnitsByFile for many paths, in one
// transaction, chunked to stay inside SQLite's parameter limit.
func (s *SQLite) DeleteASTUnitsByFiles(ctx context.Context, repoID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete ast units by files: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	err = eachPathChunk(paths, func(batch []string) error {
		args := make([]any, 0, len(batch)+1)
		args = append(args, repoID)
		for _, p := range batch {
			args = append(args, p)
		}
		in := placeholders(len(batch))
		if _, err := tx.ExecContext(ctx,
			`UPDATE edges SET dst_id = 0, dst_repo_id = ''
			 WHERE dst_id <> 0
			   AND dst_id IN (SELECT id FROM ast_units WHERE repo_id = ? AND file_path IN (`+in+`))`,
			args...,
		); err != nil {
			return fmt.Errorf("unresolve edges into files: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM ast_units WHERE repo_id = ? AND file_path IN ("+in+")", args...,
		); err != nil {
			return fmt.Errorf("delete ast units by files: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete ast units by files: %w", err)
	}
	return nil
}

// DeleteASTUnitsByKind deletes all AST units of a kind for a repository.
func (s *SQLite) DeleteASTUnitsByKind(ctx context.Context, repoID, kind string) error {
	if err := s.q.DeleteASTUnitsByKind(ctx, DeleteASTUnitsByKindParams{RepoID: repoID, Kind: kind}); err != nil {
		return fmt.Errorf("delete ast units by kind: %w", err)
	}
	return nil
}

// DeleteASTUnitsByRepo deletes all AST units for a repository.
func (s *SQLite) DeleteASTUnitsByRepo(ctx context.Context, repoID string) error {
	if err := s.q.DeleteASTUnitsByRepo(ctx, repoID); err != nil {
		return fmt.Errorf("delete ast units by repo: %w", err)
	}
	return nil
}

// BatchStoreASTUnits stores multiple AST units in a transaction, assigning
// each its new ID.
func (s *SQLite) BatchStoreASTUnits(ctx context.Context, units []*domain.ASTUnit) error {
	if len(units) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, insertASTUnit)
	if err != nil {
		return fmt.Errorf("prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, u := range units {
		var id int64
		if err := stmt.QueryRowContext(ctx,
			u.RepoID, u.FilePath, u.Language, u.Kind, u.Name, u.Qualified,
			nullInt(u.ParentID), u.StartLine, u.EndLine, u.StartByte, u.EndByte,
			u.Signature, u.Doc, u.Hash, u.Meta,
		).Scan(&id); err != nil {
			return fmt.Errorf("exec statement: %w", err)
		}
		u.ID = fmt.Sprintf("%d", id)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// GetASTUnitByName finds an AST unit by name (and optionally repo/language).
func (s *SQLite) GetASTUnitByName(ctx context.Context, repoID, name, language string) (*domain.ASTUnit, error) {
	query := "SELECT " + store.UnitColumns + " FROM ast_units WHERE name = ? AND repo_id = ?"
	args := []interface{}{name, repoID}

	if language != "" {
		query += " AND language = ?"
		args = append(args, language)
	}

	u, err := scanASTUnit(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ast unit by name: %w", err)
	}
	return u, nil
}

// GetASTUnitByQualifiedName finds an AST unit by qualified name.
func (s *SQLite) GetASTUnitByQualifiedName(ctx context.Context, repoID, qualified string) (*domain.ASTUnit, error) {
	r, err := s.q.GetASTUnitByQualifiedName(ctx, GetASTUnitByQualifiedNameParams{Qualified: qualified, RepoID: repoID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ast unit by qualified name: %w", err)
	}
	return unitFromRow(r), nil
}

// CountASTUnitsByRepo returns the number of AST units for a repository.
func (s *SQLite) CountASTUnitsByRepo(ctx context.Context, repoID string) (int64, error) {
	n, err := s.q.CountASTUnitsByRepo(ctx, repoID)
	if err != nil {
		return 0, fmt.Errorf("count ast units: %w", err)
	}
	return n, nil
}

// CountASTUnits returns the total number of stored AST units.
func (s *SQLite) CountASTUnits(ctx context.Context) (int64, error) {
	n, err := s.q.CountASTUnits(ctx)
	if err != nil {
		return 0, fmt.Errorf("count ast units: %w", err)
	}
	return n, nil
}

// GetASTUnitByID returns a single AST unit by its ID.
func (s *SQLite) GetASTUnitByID(ctx context.Context, id string) (*domain.ASTUnit, error) {
	r, err := s.q.GetASTUnitByID(ctx, intOrZero(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ast unit by id: %w", err)
	}
	return unitFromRow(r), nil
}

// GetASTUnitsByIDs returns units matching the given IDs in one query per
// chunk, to stay inside SQLite's parameter limit.
func (s *SQLite) GetASTUnitsByIDs(ctx context.Context, ids []string) ([]*domain.ASTUnit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var units []*domain.ASTUnit
	err := eachPathChunk(ids, func(batch []string) error {
		query := "SELECT " + store.UnitColumns + " FROM ast_units WHERE id IN (" + placeholders(len(batch)) + ")"
		args := make([]interface{}, 0, len(batch))
		for _, id := range batch {
			args = append(args, intOrZero(id))
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("query ast units by ids: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			u, err := scanASTUnit(rows)
			if err != nil {
				return fmt.Errorf("scan ast unit: %w", err)
			}
			units = append(units, u)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return units, nil
}
