package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanEdge(sc scanner) (*domain.Edge, error) {
	var e domain.Edge
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

func intOrZero(s string) int64 { return store.IntOrZero(s) }

// StoreEdge stores an edge, assigning its new ID back into e.
func (s *SQLite) StoreEdge(ctx context.Context, e *domain.Edge) error {
	id, err := s.q.InsertEdge(ctx, edgeInsertParams(e))
	if err != nil {
		return fmt.Errorf("store edge: %w", err)
	}
	if e.ID == "" {
		e.ID = strconv.FormatInt(id, 10)
	}
	return nil
}

// GetEdges gets edges with optional filtering. The filtering and ordering are
// storage's, shared with the Postgres backend; only running the statement is
// this backend's own.
func (s *SQLite) GetEdges(ctx context.Context, opts domain.QueryOpts) ([]*domain.Edge, error) {
	b := store.NewBuilder(store.SQLiteDialect{})
	store.EdgeFilters(b, opts, func(id string) any { return intOrZero(id) })
	var limit string
	if opts.Limit > 0 {
		limit = b.Limit(opts.Limit)
	}
	where, args := b.Where()
	query := "SELECT " + store.EdgeColumns + " FROM edges WHERE 1=1" + where + store.EdgeOrder + limit

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	defer rows.Close()

	var edges []*domain.Edge
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
	if err := s.q.DeleteEdgesByFile(ctx, DeleteEdgesByFileParams{RepoID: repoID, FilePath: filePath}); err != nil {
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
	if err := s.q.DeleteEdgesByRepo(ctx, repoID); err != nil {
		return fmt.Errorf("delete edges by repo: %w", err)
	}
	return nil
}

// DeleteEdgesByKind deletes edges of a kind for a repository (repoID empty = all repos).
func (s *SQLite) DeleteEdgesByKind(ctx context.Context, repoID, kind string) error {
	var err error
	if repoID == "" {
		err = s.q.DeleteEdgesByKindGlobal(ctx, kind)
	} else {
		err = s.q.DeleteEdgesByKind(ctx, DeleteEdgesByKindParams{RepoID: repoID, Kind: kind})
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
	if err := s.q.DeleteEdgesByKindAndDst(ctx, DeleteEdgesByKindAndDstParams{Kind: kind, DstName: dstName}); err != nil {
		return fmt.Errorf("delete edges by kind and dst: %w", err)
	}
	return nil
}

// BatchStoreEdges stores multiple edges in a transaction, assigning each its
// new ID.
func (s *SQLite) BatchStoreEdges(ctx context.Context, edges []*domain.Edge) error {
	if len(edges) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, insertEdge)
	if err != nil {
		return fmt.Errorf("prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, e := range edges {
		var id int64
		if err := stmt.QueryRowContext(ctx,
			e.RepoID, intOrZero(e.SrcID), intOrZero(e.DstID), e.Kind, e.DstName,
			e.FilePath, e.Line, e.DstRepoID, e.Confidence, e.Meta,
		).Scan(&id); err != nil {
			return fmt.Errorf("exec statement: %w", err)
		}
		e.ID = strconv.FormatInt(id, 10)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// GetEdgesBySource gets edges by source AST unit ID.
func (s *SQLite) GetEdgesBySource(ctx context.Context, repoID, srcID string, kind string) ([]*domain.Edge, error) {
	return s.GetEdges(ctx, domain.QueryOpts{RepoID: repoID, SrcID: srcID, Kind: kind})
}

// GetUnresolvedEdges gets unresolved edges (dst_id = 0). repoID empty = all repos.
func (s *SQLite) GetUnresolvedEdges(ctx context.Context, repoID string, kind string) ([]*domain.Edge, error) {
	return s.GetEdges(ctx, domain.QueryOpts{RepoID: repoID, Kind: kind, Unresolved: true})
}

// UpdateEdgeResolution sets the destination of an edge after linking.
func (s *SQLite) UpdateEdgeResolution(ctx context.Context, edgeID, dstID, dstRepoID string, confidence float32) error {
	n, err := s.q.UpdateEdgeResolution(ctx, UpdateEdgeResolutionParams{
		DstID:      intOrZero(dstID),
		DstRepoID:  dstRepoID,
		Confidence: float64(confidence),
		ID:         intOrZero(edgeID),
	})
	if err != nil {
		return fmt.Errorf("update edge resolution: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// resolutionTxRows is how many resolutions share one transaction. SQLite's
// per-commit cost is what linking used to pay per edge; a thousand rows
// amortize it away — re-measuring Elasticsearch's 2.6M resolutions at 10k,
// 50k and 200k rows per transaction bought nothing over 1k — and a bounded
// batch keeps a failed transaction's row-by-row retry cheap.
const resolutionTxRows = 1000

// BatchUpdateEdgeResolutions applies resolutions in transactions of
// resolutionTxRows rows each.
func (s *SQLite) BatchUpdateEdgeResolutions(ctx context.Context, res []store.EdgeResolution) ([]store.EdgeResolutionFailure, error) {
	var failures []store.EdgeResolutionFailure
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
func (s *SQLite) resolutionTx(ctx context.Context, offset int, chunk []store.EdgeResolution) ([]store.EdgeResolutionFailure, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, updateEdgeResolution)
	if err != nil {
		return nil, fmt.Errorf("prepare statement: %w", err)
	}
	defer stmt.Close()

	var failures []store.EdgeResolutionFailure
	for i, r := range chunk {
		out, err := stmt.ExecContext(ctx, intOrZero(r.DstID), r.DstRepoID, r.Confidence, intOrZero(r.EdgeID))
		if err != nil {
			return nil, fmt.Errorf("update edge resolution: %w", err)
		}
		if n, _ := out.RowsAffected(); n == 0 {
			failures = append(failures, store.EdgeResolutionFailure{
				Index: offset + i, EdgeID: r.EdgeID, Err: store.ErrNotFound,
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
func (s *SQLite) resolutionRows(ctx context.Context, offset int, chunk []store.EdgeResolution) []store.EdgeResolutionFailure {
	var failures []store.EdgeResolutionFailure
	for i, r := range chunk {
		if err := s.UpdateEdgeResolution(ctx, r.EdgeID, r.DstID, r.DstRepoID, r.Confidence); err != nil {
			failures = append(failures, store.EdgeResolutionFailure{
				Index: offset + i, EdgeID: r.EdgeID, Err: err,
			})
		}
	}
	return failures
}

// UpdateEdgeDstName rewrites the destination join key of an edge (used when
// the linker resolves config references like topic:${ORDERS_TOPIC}).
func (s *SQLite) UpdateEdgeDstName(ctx context.Context, edgeID, dstName string) error {
	n, err := s.q.UpdateEdgeDstName(ctx, UpdateEdgeDstNameParams{DstName: dstName, ID: intOrZero(edgeID)})
	if err != nil {
		return fmt.Errorf("update edge dst_name: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// UpdateEdgeMeta rewrites an edge's metadata in place.
func (s *SQLite) UpdateEdgeMeta(ctx context.Context, edgeID, meta string) error {
	n, err := s.q.UpdateEdgeMeta(ctx, UpdateEdgeMetaParams{Meta: meta, ID: intOrZero(edgeID)})
	if err != nil {
		return fmt.Errorf("update edge meta: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ResolveEdges resolves unresolved edges by updating dst_id.
func (s *SQLite) ResolveEdges(ctx context.Context, repoID, dstName, kind string, dstID string) error {
	if err := s.q.ResolveEdges(ctx, ResolveEdgesParams{
		DstID: intOrZero(dstID), RepoID: repoID, DstName: dstName, Kind: kind,
	}); err != nil {
		return fmt.Errorf("resolve edges: %w", err)
	}
	return nil
}

// GetEdgesByDestination gets edges by destination AST unit ID.
func (s *SQLite) GetEdgesByDestination(ctx context.Context, repoID, dstID string, kind string) ([]*domain.Edge, error) {
	return s.GetEdges(ctx, domain.QueryOpts{RepoID: repoID, DstID: dstID, Kind: kind})
}

// GetEdge returns a single edge by ID.
func (s *SQLite) GetEdge(ctx context.Context, id string) (*domain.Edge, error) {
	r, err := s.q.GetEdge(ctx, intOrZero(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get edge: %w", err)
	}
	return edgeFromRow(r), nil
}
