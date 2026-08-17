package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/storage/sqlutil"
)

const unitColumns = sqlutil.UnitColumns

func scanASTUnit(sc scanner) (*storage.ASTUnit, error) {
	var u storage.ASTUnit
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

// StoreASTUnit stores an AST unit.
func (s *SQLite) StoreASTUnit(ctx context.Context, u *storage.ASTUnit) error {

	query := `
		INSERT INTO ast_units (repo_id, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash, meta)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	result, err := s.db.ExecContext(ctx, query,
		u.RepoID, u.FilePath, u.Language, u.Kind, u.Name, u.Qualified,
		nullInt(u.ParentID), u.StartLine, u.EndLine, u.StartByte, u.EndByte,
		u.Signature, u.Doc, u.Hash, u.Meta,
	)
	if err != nil {
		return fmt.Errorf("store ast unit: %w", err)
	}

	// Set ID if it's a new unit
	if u.ID == "" {
		id, _ := result.LastInsertId()
		u.ID = fmt.Sprintf("%d", id)
	}

	return nil
}

// GetASTUnits gets AST units with optional filtering. The filtering, tiering
// and ranking are sqlutil's, shared with the Postgres backend; only running the
// statement is this backend's own.
func (s *SQLite) GetASTUnits(ctx context.Context, opts storage.QueryOpts) ([]*storage.ASTUnit, error) {
	return sqlutil.QueryUnits(sqlutil.SQLiteDialect{}, opts,
		func(id string) any { return intOrZero(id) },
		func(query string, args []any) ([]*storage.ASTUnit, error) {
			rows, err := s.db.QueryContext(ctx, query, args...)
			if err != nil {
				return nil, fmt.Errorf("query ast units: %w", err)
			}
			defer rows.Close()

			var units []*storage.ASTUnit
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

// DeleteASTUnitsByFile deletes all AST units for a file and unresolves the
// edges that pointed at them.
//
// Re-indexing a file deletes and recreates its units with new IDs. An edge
// pointing *into* the file keeps the old dst_id, which resolves to nothing yet
// still reads as resolved to every caller testing dst_id != "" — a call edge
// that silently targets a symbol that no longer exists. Clearing the
// resolution hands the edge back to the linker, which re-resolves it against
// the new units on its next run.
//
// Both statements run in one transaction: an unresolve without the delete (or
// the other way round) is a worse state than either.
func (s *SQLite) DeleteASTUnitsByFile(ctx context.Context, repoID, filePath string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete ast units by file: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The subquery is served by idx_ast_units_repo_file and the outer update by
	// idx_edges_dst, so the cost is proportional to the file's units and their
	// referrers rather than to the size of the edge table.
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
// transaction: an index pass rewrites a whole window of files, and the commits
// rather than the deleted rows are what that used to cost.
//
// The paths are chunked because SQLite binds a bounded number of parameters per
// statement; the chunks share the transaction, so the unresolve and the delete
// still either both happen or neither does. Within a chunk the unresolve runs
// first, for the reason DeleteASTUnitsByFile documents — and across chunks the
// order is equally safe, since a chunk only ever unresolves against units that
// no earlier chunk touched.
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
	_, err := s.db.ExecContext(ctx, "DELETE FROM ast_units WHERE repo_id = ? AND kind = ?", repoID, kind)
	if err != nil {
		return fmt.Errorf("delete ast units by kind: %w", err)
	}
	return nil
}

// DeleteASTUnitsByRepo deletes all AST units for a repository.
func (s *SQLite) DeleteASTUnitsByRepo(ctx context.Context, repoID string) error {

	_, err := s.db.ExecContext(ctx, "DELETE FROM ast_units WHERE repo_id = ?", repoID)
	if err != nil {
		return fmt.Errorf("delete ast units by repo: %w", err)
	}
	return nil
}

// BatchStoreASTUnits stores multiple AST units in a transaction.
func (s *SQLite) BatchStoreASTUnits(ctx context.Context, units []*storage.ASTUnit) error {
	if len(units) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `
		INSERT INTO ast_units (repo_id, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash, meta)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, u := range units {
		result, err := stmt.ExecContext(ctx,
			u.RepoID, u.FilePath, u.Language, u.Kind, u.Name, u.Qualified,
			nullInt(u.ParentID), u.StartLine, u.EndLine, u.StartByte, u.EndByte,
			u.Signature, u.Doc, u.Hash, u.Meta,
		)
		if err != nil {
			return fmt.Errorf("exec statement: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return fmt.Errorf("last insert id: %w", err)
		}
		u.ID = fmt.Sprintf("%d", id)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// GetASTUnitByName finds an AST unit by name (and optionally repo/language).
func (s *SQLite) GetASTUnitByName(ctx context.Context, repoID, name, language string) (*storage.ASTUnit, error) {

	query := "SELECT " + unitColumns + " FROM ast_units WHERE name = ? AND repo_id = ?"
	args := []interface{}{name, repoID}

	if language != "" {
		query += " AND language = ?"
		args = append(args, language)
	}

	row := s.db.QueryRowContext(ctx, query, args...)

	u, err := scanASTUnit(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ast unit by name: %w", err)
	}

	return u, nil
}

// GetASTUnitByQualifiedName finds an AST unit by qualified name.
func (s *SQLite) GetASTUnitByQualifiedName(ctx context.Context, repoID, qualified string) (*storage.ASTUnit, error) {

	query := "SELECT " + unitColumns + " FROM ast_units WHERE qualified = ? AND repo_id = ?"
	row := s.db.QueryRowContext(ctx, query, qualified, repoID)

	u, err := scanASTUnit(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ast unit by qualified name: %w", err)
	}

	return u, nil
}

// CountASTUnitsByRepo returns the number of AST units for a repository.
func (s *SQLite) CountASTUnitsByRepo(ctx context.Context, repoID string) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ast_units WHERE repo_id = ?", repoID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count ast units: %w", err)
	}
	return count, nil
}

// CountASTUnits returns the total number of stored AST units.
func (s *SQLite) CountASTUnits(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ast_units").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count ast units: %w", err)
	}
	return count, nil
}

// GetASTUnitByID returns a single AST unit by its ID.
func (s *SQLite) GetASTUnitByID(ctx context.Context, id string) (*storage.ASTUnit, error) {
	query := "SELECT " + unitColumns + " FROM ast_units WHERE id = ?"
	row := s.db.QueryRowContext(ctx, query, id)
	u, err := scanASTUnit(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ast unit by id: %w", err)
	}
	return u, nil
}

// GetASTUnitsByIDs returns units matching the given IDs in a single query.
func (s *SQLite) GetASTUnitsByIDs(ctx context.Context, ids []string) ([]*storage.ASTUnit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	query := "SELECT " + unitColumns + " FROM ast_units WHERE id IN (?" + strings.Repeat(",?", len(ids)-1) + ")"
	args := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		args = append(args, intOrZero(id))
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query ast units by ids: %w", err)
	}
	defer rows.Close()

	var units []*storage.ASTUnit
	for rows.Next() {
		u, err := scanASTUnit(rows)
		if err != nil {
			return nil, fmt.Errorf("scan ast unit: %w", err)
		}
		units = append(units, u)
	}
	return units, rows.Err()
}

// Helper functions

func nullInt(s string) sql.NullInt64 {
	if s == "" {
		return sql.NullInt64{}
	}
	// Try to parse as int64
	var i int64
	if _, err := fmt.Sscanf(s, "%d", &i); err == nil {
		return sql.NullInt64{Int64: i, Valid: true}
	}
	return sql.NullInt64{}
}
