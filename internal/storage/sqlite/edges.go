package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/storage/sqlutil"
)

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

const edgeColumns = sqlutil.EdgeColumns

func scanEdge(sc scanner) (*storage.Edge, error) {
	var e storage.Edge
	var dstID int64
	err := sc.Scan(
		&e.ID, &e.RepoID, &e.SrcID, &dstID, &e.Kind, &e.DstName,
		&e.FilePath, &e.Line, &e.DstRepoID, &e.Confidence, &e.Meta,
	)
	if err != nil {
		return nil, err
	}
	if dstID != 0 {
		e.DstID = strconv.FormatInt(dstID, 10)
	}
	return &e, nil
}

// intOrZero converts a string unit ID to its integer form; empty -> 0 (unresolved).
func intOrZero(s string) int64 {
	if s == "" {
		return 0
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return i
}

// StoreEdge stores an edge.
func (s *SQLite) StoreEdge(ctx context.Context, e *storage.Edge) error {

	query := `
		INSERT INTO edges (repo_id, src_id, dst_id, kind, dst_name, file_path, line, dst_repo_id, confidence, meta)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	result, err := s.db.ExecContext(ctx, query,
		e.RepoID, intOrZero(e.SrcID), intOrZero(e.DstID), e.Kind, e.DstName,
		e.FilePath, e.Line, e.DstRepoID, e.Confidence, e.Meta,
	)
	if err != nil {
		return fmt.Errorf("store edge: %w", err)
	}

	// Set ID if it's a new edge
	if e.ID == "" {
		id, _ := result.LastInsertId()
		e.ID = strconv.FormatInt(id, 10)
	}

	return nil
}

// GetEdges gets edges with optional filtering.
func (s *SQLite) GetEdges(ctx context.Context, opts storage.QueryOpts) ([]*storage.Edge, error) {

	b := sqlutil.NewBuilder(sqlutil.SQLiteDialect{})
	sqlutil.EdgeFilters(b, opts, func(id string) any { return intOrZero(id) })
	var limit string
	if opts.Limit > 0 {
		limit = b.Limit(opts.Limit)
	}
	where, args := b.Where()
	query := "SELECT " + edgeColumns + " FROM edges WHERE 1=1" + where + sqlutil.EdgeOrder + limit

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	defer rows.Close()

	var edges []*storage.Edge
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		edges = append(edges, e)
	}

	return edges, rows.Err()
}

// DeleteEdgesByFile deletes all edges for a file.
func (s *SQLite) DeleteEdgesByFile(ctx context.Context, repoID, filePath string) error {

	_, err := s.db.ExecContext(ctx, "DELETE FROM edges WHERE repo_id = ? AND file_path = ?", repoID, filePath)
	if err != nil {
		return fmt.Errorf("delete edges by file: %w", err)
	}
	return nil
}

// DeleteEdgesByFiles deletes the edges of many files of one repo in one
// transaction, chunked to stay inside SQLite's parameter limit.
func (s *SQLite) DeleteEdgesByFiles(ctx context.Context, repoID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete edges by files: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	err = eachPathChunk(paths, func(batch []string) error {
		args := make([]any, 0, len(batch)+1)
		args = append(args, repoID)
		for _, p := range batch {
			args = append(args, p)
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM edges WHERE repo_id = ? AND file_path IN ("+placeholders(len(batch))+")", args...,
		); err != nil {
			return fmt.Errorf("delete edges by files: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete edges by files: %w", err)
	}
	return nil
}

// DeleteEdgesByRepo deletes all edges for a repository.
func (s *SQLite) DeleteEdgesByRepo(ctx context.Context, repoID string) error {

	_, err := s.db.ExecContext(ctx, "DELETE FROM edges WHERE repo_id = ?", repoID)
	if err != nil {
		return fmt.Errorf("delete edges by repo: %w", err)
	}
	return nil
}

// DeleteEdgesByKind deletes edges of a kind for a repository (repoID empty = all repos).
func (s *SQLite) DeleteEdgesByKind(ctx context.Context, repoID, kind string) error {
	var err error
	if repoID == "" {
		_, err = s.db.ExecContext(ctx, "DELETE FROM edges WHERE kind = ?", kind)
	} else {
		_, err = s.db.ExecContext(ctx, "DELETE FROM edges WHERE repo_id = ? AND kind = ?", repoID, kind)
	}
	if err != nil {
		return fmt.Errorf("delete edges by kind: %w", err)
	}
	return nil
}

// DeleteEdgesByKindAndFiles deletes edges of a kind that originate in the
// given files. An incremental pass regenerates only the files it was handed,
// so deleting the whole repo's edges of that kind would drop data the pass
// never rebuilds.
func (s *SQLite) DeleteEdgesByKindAndFiles(ctx context.Context, repoID, kind string, filePaths []string) error {
	return eachPathChunk(filePaths, func(batch []string) error {
		args := make([]any, 0, len(batch)+2)
		args = append(args, repoID, kind)
		for _, p := range batch {
			args = append(args, p)
		}
		query := "DELETE FROM edges WHERE repo_id = ? AND kind = ? AND file_path IN (" +
			placeholders(len(batch)) + ")"
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("delete edges by kind and files: %w", err)
		}
		return nil
	})
}

// pathChunk is how many path arguments one statement binds; SQLite's default
// parameter limit is 999, so this stays well under it with room for the
// statement's own arguments.
const pathChunk = 400

// eachPathChunk calls fn with successive chunks of paths, and not at all when
// there are none.
func eachPathChunk(paths []string, fn func([]string) error) error {
	for start := 0; start < len(paths); start += pathChunk {
		if err := fn(paths[start:min(start+pathChunk, len(paths))]); err != nil {
			return err
		}
	}
	return nil
}

// placeholders returns "?,?,..." for n arguments.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return "?" + strings.Repeat(",?", n-1)
}

// DeleteEdgesByKindAndDst deletes edges of a kind pointing at a destination name.
func (s *SQLite) DeleteEdgesByKindAndDst(ctx context.Context, kind, dstName string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM edges WHERE kind = ? AND dst_name = ?", kind, dstName)
	if err != nil {
		return fmt.Errorf("delete edges by kind and dst: %w", err)
	}
	return nil
}

// BatchStoreEdges stores multiple edges in a transaction.
func (s *SQLite) BatchStoreEdges(ctx context.Context, edges []*storage.Edge) error {
	if len(edges) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `
		INSERT INTO edges (repo_id, src_id, dst_id, kind, dst_name, file_path, line, dst_repo_id, confidence, meta)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, e := range edges {
		result, err := stmt.ExecContext(ctx,
			e.RepoID, intOrZero(e.SrcID), intOrZero(e.DstID), e.Kind, e.DstName,
			e.FilePath, e.Line, e.DstRepoID, e.Confidence, e.Meta,
		)
		if err != nil {
			return fmt.Errorf("exec statement: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return fmt.Errorf("last insert id: %w", err)
		}
		e.ID = strconv.FormatInt(id, 10)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// GetEdgesBySource gets edges by source AST unit ID.
func (s *SQLite) GetEdgesBySource(ctx context.Context, repoID, srcID string, kind string) ([]*storage.Edge, error) {
	return s.GetEdges(ctx, storage.QueryOpts{RepoID: repoID, SrcID: srcID, Kind: kind})
}

// GetUnresolvedEdges gets unresolved edges (dst_id = 0). repoID empty = all repos.
func (s *SQLite) GetUnresolvedEdges(ctx context.Context, repoID string, kind string) ([]*storage.Edge, error) {
	return s.GetEdges(ctx, storage.QueryOpts{RepoID: repoID, Kind: kind, Unresolved: true})
}

// UpdateEdgeResolution sets the destination of an edge after linking.
func (s *SQLite) UpdateEdgeResolution(ctx context.Context, edgeID, dstID, dstRepoID string, confidence float32) error {
	res, err := s.db.ExecContext(ctx, updateEdgeResolutionSQL,
		intOrZero(dstID), dstRepoID, confidence, intOrZero(edgeID),
	)
	if err != nil {
		return fmt.Errorf("update edge resolution: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// updateEdgeResolutionSQL is shared by the single-row and batched paths so the
// two cannot drift apart.
const updateEdgeResolutionSQL = `UPDATE edges SET dst_id = ?, dst_repo_id = ?, confidence = ? WHERE id = ?`

// resolutionTxRows is how many resolutions share one transaction. SQLite's
// per-commit cost is what linking used to pay per edge; a thousand rows
// amortize it away — re-measuring Elasticsearch's 2.6M resolutions at 10k,
// 50k and 200k rows per transaction bought nothing over 1k — and a bounded
// batch keeps a failed transaction's row-by-row retry cheap.
const resolutionTxRows = 1000

// BatchUpdateEdgeResolutions applies resolutions in transactions of
// resolutionTxRows rows each.
func (s *SQLite) BatchUpdateEdgeResolutions(ctx context.Context, res []storage.EdgeResolution) ([]storage.EdgeResolutionFailure, error) {
	var failures []storage.EdgeResolutionFailure
	for start := 0; start < len(res); start += resolutionTxRows {
		chunk := res[start:min(start+resolutionTxRows, len(res))]
		got, err := s.resolutionTx(ctx, start, chunk)
		if err != nil {
			if ctx.Err() != nil {
				return failures, err
			}
			// The transaction failed as a whole, which says nothing about
			// which row broke it. Retry the chunk row by row so one bad
			// resolution costs only itself.
			got = s.resolutionRows(ctx, start, chunk)
		}
		failures = append(failures, got...)
	}
	return failures, nil
}

// resolutionTx applies one chunk in a single transaction. A row that matched
// nothing is reported as a failure but does not abort the transaction: a
// deleted edge is not a reason to drop the other 999 updates.
func (s *SQLite) resolutionTx(ctx context.Context, offset int, chunk []storage.EdgeResolution) ([]storage.EdgeResolutionFailure, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, updateEdgeResolutionSQL)
	if err != nil {
		return nil, fmt.Errorf("prepare statement: %w", err)
	}
	defer stmt.Close()

	var failures []storage.EdgeResolutionFailure
	for i, r := range chunk {
		out, err := stmt.ExecContext(ctx, intOrZero(r.DstID), r.DstRepoID, r.Confidence, intOrZero(r.EdgeID))
		if err != nil {
			return nil, fmt.Errorf("update edge resolution: %w", err)
		}
		if n, _ := out.RowsAffected(); n == 0 {
			failures = append(failures, storage.EdgeResolutionFailure{
				Index: offset + i, EdgeID: r.EdgeID, Err: storage.ErrNotFound,
			})
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}
	return failures, nil
}

// resolutionRows applies a chunk one autocommit statement at a time, the way
// linking used to, and reports every row that failed.
func (s *SQLite) resolutionRows(ctx context.Context, offset int, chunk []storage.EdgeResolution) []storage.EdgeResolutionFailure {
	var failures []storage.EdgeResolutionFailure
	for i, r := range chunk {
		if err := s.UpdateEdgeResolution(ctx, r.EdgeID, r.DstID, r.DstRepoID, r.Confidence); err != nil {
			failures = append(failures, storage.EdgeResolutionFailure{
				Index: offset + i, EdgeID: r.EdgeID, Err: err,
			})
		}
	}
	return failures
}

// UpdateEdgeDstName rewrites the destination join key of an edge (used when
// the linker resolves config references like topic:${ORDERS_TOPIC}).
func (s *SQLite) UpdateEdgeDstName(ctx context.Context, edgeID, dstName string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_name = ? WHERE id = ?`, dstName, intOrZero(edgeID))
	if err != nil {
		return fmt.Errorf("update edge dst_name: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// UpdateEdgeMeta rewrites an edge's metadata in place.
func (s *SQLite) UpdateEdgeMeta(ctx context.Context, edgeID, meta string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE edges SET meta = ? WHERE id = ?`, meta, intOrZero(edgeID))
	if err != nil {
		return fmt.Errorf("update edge meta: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// ResolveEdges resolves unresolved edges by updating dst_id.
func (s *SQLite) ResolveEdges(ctx context.Context, repoID, dstName, kind string, dstID string) error {

	query := `
		UPDATE edges
		SET dst_id = ?
		WHERE dst_id = 0 AND repo_id = ? AND dst_name = ? AND kind = ?
	`
	_, err := s.db.ExecContext(ctx, query, intOrZero(dstID), repoID, dstName, kind)
	if err != nil {
		return fmt.Errorf("resolve edges: %w", err)
	}
	return nil
}

// GetEdgesByDestination gets edges by destination AST unit ID.
func (s *SQLite) GetEdgesByDestination(ctx context.Context, repoID, dstID string, kind string) ([]*storage.Edge, error) {
	return s.GetEdges(ctx, storage.QueryOpts{RepoID: repoID, DstID: dstID, Kind: kind})
}

// GetEdge returns a single edge by ID.
func (s *SQLite) GetEdge(ctx context.Context, id string) (*storage.Edge, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+edgeColumns+" FROM edges WHERE id = ?", intOrZero(id))
	e, err := scanEdge(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get edge: %w", err)
	}
	return e, nil
}
