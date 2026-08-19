package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// notFound maps sql.ErrNoRows to store.ErrNotFound and wraps any other
// error with op.
func notFound(err error, op string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// b2i and i2b convert between Go bool and SQLite's INTEGER 0/1, which is how
// the schema stores booleans.
func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func i2b(i int64) bool { return i != 0 }

func fileFromRow(r File) *store.File {
	return &store.File{
		RepoID: r.RepoID, Path: r.Path, Language: r.Language, Hash: r.Hash,
		Size: r.Size, ModTime: r.ModTime, Indexed: i2b(r.Indexed),
	}
}

func unitFromRow(r AstUnit) *domain.ASTUnit {
	u := &domain.ASTUnit{
		ID:        strconv.FormatInt(r.ID, 10),
		RepoID:    r.RepoID,
		FilePath:  r.FilePath,
		Language:  r.Language,
		Kind:      r.Kind,
		Name:      r.Name,
		Qualified: r.Qualified,
		StartLine: int(r.StartLine),
		EndLine:   int(r.EndLine),
		StartByte: int(r.StartByte),
		EndByte:   int(r.EndByte),
		Signature: r.Signature,
		Doc:       r.Doc,
		Hash:      r.Hash,
		Meta:      r.Meta,
	}
	if r.ParentID.Valid {
		u.ParentID = strconv.FormatInt(r.ParentID.Int64, 10)
	}
	return u
}

func unitInsertParams(u *domain.ASTUnit) InsertASTUnitParams {
	return InsertASTUnitParams{
		RepoID:    u.RepoID,
		FilePath:  u.FilePath,
		Language:  u.Language,
		Kind:      u.Kind,
		Name:      u.Name,
		Qualified: u.Qualified,
		ParentID:  nullInt(u.ParentID),
		StartLine: int64(u.StartLine),
		EndLine:   int64(u.EndLine),
		StartByte: int64(u.StartByte),
		EndByte:   int64(u.EndByte),
		Signature: u.Signature,
		Doc:       u.Doc,
		Hash:      u.Hash,
		Meta:      u.Meta,
	}
}

func edgeFromRow(r Edge) *domain.Edge {
	e := &domain.Edge{
		ID:         strconv.FormatInt(r.ID, 10),
		RepoID:     r.RepoID,
		SrcID:      strconv.FormatInt(r.SrcID, 10),
		Kind:       r.Kind,
		DstName:    r.DstName,
		FilePath:   r.FilePath,
		Line:       int(r.Line),
		DstRepoID:  r.DstRepoID,
		Confidence: float32(r.Confidence),
		Meta:       r.Meta,
	}
	if r.DstID != 0 {
		e.DstID = strconv.FormatInt(r.DstID, 10)
	}
	return e
}

func edgeInsertParams(e *domain.Edge) InsertEdgeParams {
	return InsertEdgeParams{
		RepoID:     e.RepoID,
		SrcID:      intOrZero(e.SrcID),
		DstID:      intOrZero(e.DstID),
		Kind:       e.Kind,
		DstName:    e.DstName,
		FilePath:   e.FilePath,
		Line:       int64(e.Line),
		DstRepoID:  e.DstRepoID,
		Confidence: float64(e.Confidence),
		Meta:       e.Meta,
	}
}

func repoFromRow(r GetRepoRow) *domain.Repo {
	return &domain.Repo{
		ID:            r.ID,
		Name:          r.Name,
		Source:        domain.SourceType(r.Source),
		URL:           r.Url,
		Path:          r.Path,
		Branch:        r.Branch,
		Status:        domain.Status(r.Status),
		LastError:     r.LastError,
		CreatedAt:     r.CreatedAt,
		IndexedAt:     r.IndexedAt,
		LastCommit:    r.LastCommit,
		PendingCommit: r.PendingCommit,
		Active:        i2b(r.Active),
	}
}

func jobFromRow(r GetIndexJobRow) *domain.IndexJob {
	return &domain.IndexJob{
		ID:          strconv.FormatInt(r.ID, 10),
		RepoID:      r.RepoID,
		Kind:        r.Kind,
		Force:       i2b(r.Force),
		Status:      r.Status,
		Error:       r.Error,
		CreatedAt:   r.CreatedAt,
		ClaimedAt:   r.ClaimedAt,
		HeartbeatAt: r.HeartbeatAt,
		ClaimedBy:   r.ClaimedBy,
	}
}

// nullInt converts a string parent ID to its nullable form; empty/unparsable
// becomes NULL.
func nullInt(s string) sql.NullInt64 {
	if s == "" {
		return sql.NullInt64{}
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: i, Valid: true}
}
