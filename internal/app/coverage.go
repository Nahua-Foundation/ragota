package app

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// CoverageReporter is the transport between the layer that recognizes
// outbound contracts (the AST indexer) and this one, which persists and
// serves the result. It is satisfied either by an indexer's per-run
// *index.IndexResult or by the indexer itself, whichever the producing
// layer finds natural; both are consulted per batch.
//
// Counting is the producer's job: only it sees a call site that looks like an
// outbound contract but yields no edge (an HTTP call whose URL is built by a
// helper, a publish whose topic is a variable). Nothing downstream can
// reconstruct those — they leave no row behind — which is why they travel
// with the run rather than being derived from the stored edges.
type CoverageReporter interface {
	// ContractCoverage returns, keyed by contract kind (domain.ContractKindHTTP
	// and friends), how many call sites looked like an outbound contract of
	// that kind and how many of them produced an edge. Edges must never exceed
	// Candidates: every edge starts as a candidate.
	ContractCoverage() map[string]domain.CoverageCounts
}

// coverageAccumulator sums the per-batch counters of one index pass. A full
// pass indexes in batches and runs several indexers over each batch, so the
// repo-wide summary only exists once they are added up.
type coverageAccumulator struct {
	kinds map[string]domain.CoverageCounts
	// reported records that at least one indexer answered at all. It is not
	// the same as a non-empty tally: an indexer that reports zero candidates
	// everywhere is saying "this repo has nothing to find", while an indexer
	// that cannot report is saying nothing, and the two must not be served as
	// the same answer.
	reported bool
}

// add merges one indexer's per-run counters.
func (a *coverageAccumulator) add(counts map[string]domain.CoverageCounts) {
	a.reported = true
	if a.kinds == nil {
		a.kinds = make(map[string]domain.CoverageCounts, len(counts))
	}
	for kind, c := range counts {
		acc := a.kinds[kind]
		acc.Candidates += c.Candidates
		acc.Edges += c.Edges
		a.kinds[kind] = acc
	}
}

// collect takes the coverage of one indexer run from whichever of the two
// carriers provides it.
//
// A carrier that satisfies the interface but answers nil has not reported: the
// per-run result type carries the coverage field for every indexer, including
// the ones that fill it in on the indexer instead, so "implements it" and
// "answered" are not the same question here.
func (a *coverageAccumulator) collect(indexer, result any) {
	for _, src := range []any{result, indexer} {
		r, ok := src.(CoverageReporter)
		if !ok {
			continue
		}
		if counts := r.ContractCoverage(); counts != nil {
			a.add(counts)
			return
		}
	}
}

// summary renders the accumulated counters for persistence, or nil when no
// indexer reported.
func (a *coverageAccumulator) summary(repoID string, at time.Time) *domain.RepoCoverage {
	if !a.reported {
		return nil
	}
	kinds := make(map[string]domain.CoverageCounts, len(a.kinds))
	for kind, c := range a.kinds {
		kinds[kind] = c
	}
	return &domain.RepoCoverage{RepoID: repoID, UpdatedAt: at.Unix(), Kinds: kinds}
}

// storeCoverage persists the summary of a finished pass. complete says that
// the pass saw the whole repository; a partial one keeps the previous summary,
// which is stale but true, rather than replacing it with counters that only
// describe the files that happened to change.
//
// A summary that cannot be written is logged rather than failing the pass: the
// files are indexed either way, and the stored UpdatedAt is what tells a
// reader that what they are looking at predates the last index.
func (s *Service) storeCoverage(ctx context.Context, repoID string, acc *coverageAccumulator, complete bool) {
	summary := acc.summary(repoID, time.Now())
	if summary == nil {
		return
	}
	if !complete {
		slog.Info("contract coverage left unchanged by a partial pass", "repo_id", repoID)
		return
	}
	if err := s.store.StoreRepoCoverage(ctx, summary); err != nil {
		// A write that lost its context to shutdown is not a fault worth
		// reporting: the pass it summarises did not finish either, and the
		// coverage is recomputed by the next one.
		if !errors.Is(err, context.Canceled) {
			slog.Warn("store contract coverage", "repo_id", repoID, "err", err)
		}
		return
	}
	slog.Info("contract coverage", "repo_id", repoID, "kinds", len(summary.Kinds))
}

// CoverageKind is one contract kind's line of a coverage report.
type CoverageKind struct {
	Kind       string `json:"kind"`
	Candidates int    `json:"candidates"`
	Edges      int    `json:"edges"`
	// Ratio is Edges/Candidates, and 1 when there were no candidates: a
	// repository with nothing of this kind to find is fully covered, not
	// uncovered.
	Ratio float64 `json:"ratio"`
}

// CoverageReport answers, per contract kind, whether an empty result means
// there was nothing to find or that the indexer missed it.
type CoverageReport struct {
	RepoID string `json:"repo_id"`
	// Reported is false when no coverage-reporting index pass has run for this
	// repo; the counters are then meaningless rather than zero.
	Reported bool `json:"reported"`
	// UpdatedAt is when the summary was written, IndexedAt when the repo last
	// finished index. A summary older than the index is stale.
	UpdatedAt int64          `json:"updated_at,omitempty"`
	IndexedAt int64          `json:"indexed_at"`
	Kinds     []CoverageKind `json:"kinds"`
	Totals    CoverageKind   `json:"totals"`
}

// RepoCoverage returns the repo's contract coverage summary. An unknown repo
// is an error (store.ErrNotFound); a known repo without a summary is not —
// it is reported as unavailable.
func (s *Service) RepoCoverage(ctx context.Context, repoID string) (*CoverageReport, error) {
	repo, err := s.store.GetRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	report := &CoverageReport{RepoID: repo.ID, IndexedAt: repo.IndexedAt, Kinds: []CoverageKind{}}

	cov, err := s.store.GetRepoCoverage(ctx, repoID)
	if errors.Is(err, store.ErrNotFound) {
		return report, nil
	}
	if err != nil {
		return nil, err
	}

	report.Reported = true
	report.UpdatedAt = cov.UpdatedAt
	// Every known kind is reported, including the ones the pass never saw: a
	// kind missing from the response would be indistinguishable from a kind
	// the indexer does not look for, which is the confusion this endpoint
	// exists to remove.
	seen := make(map[string]bool, len(cov.Kinds))
	for _, kind := range domain.ContractKinds {
		seen[kind] = true
		report.Kinds = append(report.Kinds, coverageKind(kind, cov.Kinds[kind]))
	}
	for kind, counts := range cov.Kinds {
		if !seen[kind] {
			report.Kinds = append(report.Kinds, coverageKind(kind, counts))
		}
	}
	for _, counts := range cov.Kinds {
		report.Totals.Candidates += counts.Candidates
		report.Totals.Edges += counts.Edges
	}
	sort.Slice(report.Kinds, func(i, j int) bool { return report.Kinds[i].Kind < report.Kinds[j].Kind })
	report.Totals.Kind = "all"
	report.Totals.Ratio = coverageRatio(report.Totals.Candidates, report.Totals.Edges)
	return report, nil
}

func coverageKind(kind string, counts domain.CoverageCounts) CoverageKind {
	return CoverageKind{
		Kind:       kind,
		Candidates: counts.Candidates,
		Edges:      counts.Edges,
		Ratio:      coverageRatio(counts.Candidates, counts.Edges),
	}
}

func coverageRatio(candidates, edges int) float64 {
	if candidates <= 0 {
		return 1
	}
	return float64(edges) / float64(candidates)
}
