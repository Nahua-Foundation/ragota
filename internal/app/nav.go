package app

import (
	"context"
	"fmt"

	"github.com/Nahua-Foundation/ragota/internal/domain"
)

// GetASTUnits returns AST units matching the query options.
func (s *Service) GetASTUnits(ctx context.Context, opts domain.QueryOpts) ([]*domain.ASTUnit, error) {
	if s.store == nil {
		return nil, fmt.Errorf("storage not available")
	}
	return s.store.GetASTUnits(ctx, opts)
}

// GetEdges returns edges matching the query options.
func (s *Service) GetEdges(ctx context.Context, opts domain.QueryOpts) ([]*domain.Edge, error) {
	if s.store == nil {
		return nil, fmt.Errorf("storage not available")
	}
	return s.store.GetEdges(ctx, opts)
}

// Symbol query limits. A symbol lookup with no limit would stream the whole
// unit table, so an explicit default and ceiling are applied here as well as
// in the handler.
const (
	defaultSymbolLimit = 50
	maxSymbolLimit     = 500
)

// ErrNoSelector is returned when a symbol query carries no filter at all.
var ErrNoSelector = fmt.Errorf("%w: at least one of repo_id, name, kind or qualified is required", ErrBadRequest)

// Symbols returns AST units matching the query options. At least one selector
// is required, and the limit is always bounded.
//
// This is the one caller that asks for the substring fallback: it answers an
// agent that named a symbol from the wording of a question, and for it an empty
// result is a dead end where a near miss is a next step. The callers that
// resolve edges do not opt in — for them a looser match is a wrong answer
// rather than an approximate one.
//
// It is also scoped by the active set, through the same retrievalScope call
// Search makes. /search and this are two doors to one question — a sentence
// goes to one, an identifier to the other, which is what both endpoints'
// descriptions tell a model — so scoping one and not the other answers the same
// question differently depending on which door it came through. That was not
// theoretical: a run scoped to one repository answered /nav/symbol out of a
// dormant one. Sharing the decision rather than copying it is deliberate; a
// second reading of "which repositories may this read" is a second thing to
// keep in step with the flag.
func (s *Service) Symbols(ctx context.Context, opts domain.QueryOpts) ([]*domain.ASTUnit, error) {
	if opts.RepoID == "" && opts.Name == "" && opts.Kind == "" && len(opts.Kinds) == 0 &&
		opts.Qualified == "" && opts.QualifiedSuffix == "" && opts.NameOrQualified == "" &&
		opts.FilePath == "" {
		return nil, ErrNoSelector
	}
	if s.store == nil {
		// Checked before the scope is resolved, which reads the store: without
		// this the missing-storage case would panic where it used to report.
		return nil, fmt.Errorf("storage not available")
	}
	if opts.Limit <= 0 {
		opts.Limit = defaultSymbolLimit
	}
	if opts.Limit > maxSymbolLimit {
		opts.Limit = maxSymbolLimit
	}
	opts.Fallback = true

	// What the request named, in the form retrievalScope takes. A single repo_id
	// is a set of one: naming a repository is how a client reaches one that is
	// out of the way, and it must work here for the same reason it works on
	// /search.
	requested := make([]string, 0, len(opts.Repos)+1)
	requested = append(requested, opts.Repos...)
	if opts.RepoID != "" {
		requested = append(requested, opts.RepoID)
	}
	scope, none, err := s.retrievalScope(ctx, requested)
	if err != nil {
		return nil, err
	}
	if none {
		// Repositories are registered and none of them is active. That is not an
		// empty scope — an empty scope means everywhere — so it is answered with
		// nothing rather than with the whole index.
		return []*domain.ASTUnit{}, nil
	}
	if len(requested) == 0 {
		// Only a request that named nothing needs the scope written into it.
		// Where one was named, retrievalScope handed the same names straight
		// back and the options already carry them.
		opts.Repos = scope
	}
	return s.GetASTUnits(ctx, opts)
}

// maxUnitsPerLine caps how many units may contain one line. Containment is
// evaluated in SQL, so this only bounds the nesting depth at that position
// (file -> type -> method -> closure), not the size of the file.
const maxUnitsPerLine = 64

// Definition finds the AST unit at the given line in the specified file.
//
// The containment filter is part of the query rather than a pass over the
// first page of the file's units: a file can hold far more units than any page
// size, so filtering afterwards returned an empty result for every symbol late
// in a large file.
func (s *Service) Definition(ctx context.Context, repoID, filePath string, line int) (*domain.ASTUnit, error) {
	if line <= 0 {
		return nil, nil
	}
	units, err := s.GetASTUnits(ctx, domain.QueryOpts{
		RepoID: repoID, FilePath: filePath, Line: line, Limit: maxUnitsPerLine,
	})
	if err != nil {
		return nil, err
	}

	// Innermost unit wins: among the units spanning the line, the one that
	// starts last is the most specific.
	var bestUnit *domain.ASTUnit
	for _, u := range units {
		if bestUnit == nil || u.StartLine > bestUnit.StartLine {
			bestUnit = u
		}
	}

	return bestUnit, nil
}

// Reference query limits, the same shape as the symbol limits above and for the
// same reason: a hot symbol's edges are unbounded, so a lookup with no limit
// would stream the whole edge table into one response.
const (
	defaultReferenceLimit = 50
	maxReferenceLimit     = 500
)

// References finds edges that reference a symbol at the given line in the specified file.
// Resolved edges are matched by destination ID; unresolved ones by name.
//
// limit bounds the answer, not each of the two lookups: it is what a caller
// reads it as, and bounding them separately returned up to twice what was asked
// for. The resolved edges draw on the budget first and the name matches only
// fill what is left, because an edge resolved to this unit names *it* while an
// unresolved one merely shares its name — the better answer must not be crowded
// out by the weaker one.
func (s *Service) References(ctx context.Context, repoID, filePath string, line, limit int) ([]*domain.Edge, error) {
	unit, err := s.Definition(ctx, repoID, filePath, line)
	if err != nil {
		return nil, err
	}

	if unit == nil {
		return []*domain.Edge{}, nil
	}

	if limit <= 0 {
		limit = defaultReferenceLimit
	}
	if limit > maxReferenceLimit {
		limit = maxReferenceLimit
	}

	references, err := s.GetEdges(ctx, domain.QueryOpts{DstID: unit.ID, Limit: limit})
	if err != nil {
		return nil, err
	}

	if room := limit - len(references); room > 0 {
		unresolved, err := s.GetEdges(ctx, domain.QueryOpts{
			RepoID: repoID, Name: unit.Name, Unresolved: true, Limit: room,
		})
		if err != nil {
			return nil, err
		}
		references = append(references, unresolved...)
	}

	return references, nil
}
