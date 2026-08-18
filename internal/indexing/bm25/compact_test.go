package bm25

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/indexing"

	index "github.com/blevesearch/bleve_index_api"
)

// corpusFiles builds files with heavily shared vocabulary, which is what makes
// the per-segment term counts overlap: a term the whole corpus uses is counted
// once per segment that holds any of it.
//
// The files differ in length and in how often they say the words the tests ask
// about, so that they score differently. A corpus of identical documents would
// tie on every query, and bleve returns ties in the order its segments happen
// to hold them — which would fail these tests for a reason that is the
// ranker's to fix (hybrid.sortHits breaks ties on repo, path and line) rather
// than the corpus statistics'.
func corpusFiles(n int) []*indexing.FileToIndex {
	// Distinct words, drawn on in varying amounts, give each file its own term
	// frequencies without giving any of them a term nothing else has.
	vocabulary := []string{
		"owner", "pet", "vet", "clinic", "visit", "appointment", "schedule",
		"specialty", "vaccination", "invoice", "reminder", "checkup",
	}

	files := make([]*indexing.FileToIndex, 0, n)
	for i := 0; i < n; i++ {
		var body strings.Builder
		fmt.Fprintf(&body, "package clinic%d\n\n", i)
		fmt.Fprintf(&body, "// Visit%d records one visit to the clinic and maps to the visits table.\n", i)
		fmt.Fprintf(&body, "type Visit%d struct {\n\tID int\n\tPetID int\n\tDescription string\n}\n\n", i)
		fmt.Fprintf(&body, "func (v *Visit%d) Table() string { return \"visits\" }\n\n", i)

		// i%7 methods, each naming a rotating slice of the vocabulary: the term
		// counts and the document length both vary, and neither varies with the
		// order the files are indexed in.
		for m := 0; m <= i%7; m++ {
			word := vocabulary[(i+m)%len(vocabulary)]
			fmt.Fprintf(&body, "// Load%s%d loads the %s rows a visit refers to.\n", word, m, word)
			fmt.Fprintf(&body, "func Load%s%d(db *DB) ([]*Visit%d, error) {\n", word, m, i)
			fmt.Fprintf(&body, "\treturn db.Query(\"select id, pet_id, description from visits join %s on %s.id = visits.%s_id\")\n}\n\n",
				word, word, word)
		}
		files = append(files, &indexing.FileToIndex{
			Path:     fmt.Sprintf("clinic/visit%04d.go", i),
			Language: "go",
			Hash:     fmt.Sprintf("h%d", i),
			Content:  []byte(body.String()),
		})
	}
	return files
}

// buildInWindows indexes files window at a time, which is how the service
// drives an indexer and what decides the segment layout the pass leaves. The
// index is closed when the test ends.
func buildInWindows(t *testing.T, files []*indexing.FileToIndex, window int) *Indexer {
	t.Helper()
	idx := buildInWindowsAt(t, t.TempDir(), files, window)
	t.Cleanup(func() { idx.Close() })
	return idx
}

// buildInWindowsAt is buildInWindows over a named directory and without the
// cleanup, for the test that closes the index itself: bleve panics on a second
// Close rather than ignoring it.
func buildInWindowsAt(t *testing.T, dir string, files []*indexing.FileToIndex, window int) *Indexer {
	t.Helper()
	idx, err := New(&Config{Path: dir, K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for start := 0; start < len(files); start += window {
		end := min(start+window, len(files))
		batch := make([]*indexing.FileToIndex, 0, end-start)
		for _, f := range files[start:end] {
			cp := *f
			batch = append(batch, &cp)
		}
		if _, err := idx.Index(context.Background(), &indexing.IndexRequest{RepoID: "r1", Files: batch}); err != nil {
			t.Fatalf("Index: %v", err)
		}
	}
	return idx
}

// scoredHits maps each hit to its score.
//
// Keyed by chunk rather than kept in rank order on purpose: what has to
// reproduce is the score a document is given. Documents whose scores tie are
// returned in whatever order the segments hold them, which is the ranker's to
// settle — hybrid.sortHits orders equal scores by repo, path and line — and
// asserting on it here would test bleve's internal document numbering instead
// of the corpus statistics these tests are about.
func scoredHits(t *testing.T, idx *Indexer, query string, limit int) map[string]float32 {
	t.Helper()
	res, err := idx.Search(context.Background(), &indexing.SearchQuery{Query: query, Limit: limit})
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("Search(%q) returned no hits", query)
	}
	out := make(map[string]float32, len(res.Hits))
	for _, h := range res.Hits {
		out[fmt.Sprintf("%s@%d", h.FilePath, h.Line)] = h.Score
	}
	return out
}

// diffScores reports the chunks the two results disagree about, most divergent
// first, so a failure names what moved instead of dumping both rankings.
func diffScores(got, want map[string]float32) []string {
	var out []string
	for chunk, w := range want {
		g, ok := got[chunk]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s: missing, want %v", chunk, w))
		case g != w:
			out = append(out, fmt.Sprintf("%s: %v, want %v (%+.2f%%)", chunk, g, w, 100*float64(g-w)/float64(w)))
		}
	}
	for chunk, g := range got {
		if _, ok := want[chunk]; !ok {
			out = append(out, fmt.Sprintf("%s: %v, want it absent", chunk, g))
		}
	}
	sort.Strings(out)
	if len(out) > 6 {
		out = append(out[:6], fmt.Sprintf("... and %d more", len(out)-6))
	}
	return out
}

// corpusStats reads back the two numbers bleve derives its average document
// length from, so a test can name the quantity that moved.
func corpusStats(t *testing.T, idx *Indexer, field string) (cardinality int, docs uint64) {
	t.Helper()
	adv, err := idx.index.Advanced()
	if err != nil {
		t.Fatalf("Advanced: %v", err)
	}
	reader, err := adv.Reader()
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	defer reader.Close()

	docs, err = reader.DocCount()
	if err != nil {
		t.Fatalf("DocCount: %v", err)
	}
	bm25Reader, ok := reader.(index.BM25Reader)
	if !ok {
		t.Fatalf("index reader %T does not report field cardinality", reader)
	}
	cardinality, err = bm25Reader.FieldCardinality(field)
	if err != nil {
		t.Fatalf("FieldCardinality: %v", err)
	}
	return cardinality, docs
}

// Two passes over the same sources must score them the same. They do not
// unless the layout is settled: bleve's average document length is a sum of
// per-segment term counts, so it counts a shared term once per segment, and
// how many segments there are is decided by a background merger rather than by
// the input. Compacting makes the count the corpus's own distinct-term count.
func TestCompactMakesScoresIndependentOfSegmentLayout(t *testing.T) {
	files := corpusFiles(600)
	queries := []string{
		"which entity maps to the visits table",
		"load the vaccination rows a visit refers to",
		"appointment schedule reminder",
	}

	// Every matching chunk, not a top-n slice: a limit would compare which
	// members of a tie made the cut rather than what each was scored.
	const everything = 10000

	var want []map[string]float32
	var wantCardinality int
	for _, window := range []int{600, 128, 32, 7} {
		idx := buildInWindows(t, files, window)

		before, docs := corpusStats(t, idx, "_all")
		if err := idx.Compact(context.Background()); err != nil {
			t.Fatalf("window %d: Compact: %v", window, err)
		}
		after, afterDocs := corpusStats(t, idx, "_all")
		if afterDocs != docs {
			t.Errorf("window %d: compaction changed the document count: %d -> %d", window, docs, afterDocs)
		}
		t.Logf("window %3d: cardinality %d -> %d over %d documents", window, before, after, docs)

		got := make([]map[string]float32, 0, len(queries))
		for _, q := range queries {
			got = append(got, scoredHits(t, idx, q, everything))
		}
		if want == nil {
			want, wantCardinality = got, after
			continue
		}
		if after != wantCardinality {
			t.Errorf("window %d: compacted cardinality = %d, want %d — the layout still leaks into the corpus statistics",
				window, after, wantCardinality)
		}
		for qi, q := range queries {
			if diff := diffScores(got[qi], want[qi]); len(diff) > 0 {
				t.Errorf("window %d scored %q differently from window %d:\n\t%s",
					window, q, 600, strings.Join(diff, "\n\t"))
			}
		}
	}
}

// Why Compact has to exist, stated so that measuring it does not race the
// background merger.
//
// Comparing two uncompacted builds would be the obvious test and is not
// writable: the merger keeps working after the last write, so the layout being
// measured changes underfoot, and a build read a moment later has already
// become a different one. Three compacted indexes are stable, and the same
// arithmetic falls out of them — the count bleve reports for a set of
// documents is not a property of that set. Two halves that share their
// vocabulary report, between them, far more terms than the whole reports,
// because a term in both halves is counted in both. An index holding those
// halves as two segments reports exactly that sum.
//
// If bleve ever starts counting each term once across segments, the halves
// will add up to the whole and this fails — which is the signal to drop the
// compaction, not a regression to fix.
func TestSegmentLayoutStillMovesTheCorpusStatistics(t *testing.T) {
	files := corpusFiles(600)
	first, second := files[:300], files[300:]

	compacted := func(files []*indexing.FileToIndex) (int, uint64) {
		idx := buildInWindows(t, files, 64)
		if err := idx.Compact(context.Background()); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		return corpusStats(t, idx, "_all")
	}

	firstCard, firstDocs := compacted(first)
	secondCard, secondDocs := compacted(second)
	wholeCard, wholeDocs := compacted(files)

	if firstDocs+secondDocs != wholeDocs {
		t.Fatalf("the halves hold %d+%d documents, the whole %d", firstDocs, secondDocs, wholeDocs)
	}
	// math.Ceil(fieldCardinality / docCount), in bleve's search_term.go.
	avgDocLength := func(cardinality int, docs uint64) int { return (cardinality + int(docs) - 1) / int(docs) }

	split, whole := firstCard+secondCard, wholeCard
	t.Logf("%d documents report %d terms in one segment and %d in two (%+.0f%%): "+
		"average document length %d against %d",
		wholeDocs, whole, split, 100*float64(split-whole)/float64(whole),
		avgDocLength(whole, wholeDocs), avgDocLength(split, wholeDocs))

	if split <= whole {
		t.Errorf("the halves report %d terms and the whole %d; bleve no longer counts terms "+
			"per segment, so Compact may no longer be needed for reproducible scores", split, whole)
	}
	if avgDocLength(split, wholeDocs) == avgDocLength(whole, wholeDocs) {
		t.Logf("this corpus happens to round to the same average document length either way; " +
			"a corpus that does not moves every score at once, which is why re-indexing noise " +
			"is intermittent rather than constant")
	}
}

// Compaction has to be safe to call on an index that is already one segment,
// because every full pass ends with it.
func TestCompactIsIdempotent(t *testing.T) {
	files := corpusFiles(200)
	idx := buildInWindows(t, files, 32)
	const query = "load the vaccination rows a visit refers to"

	if err := idx.Compact(context.Background()); err != nil {
		t.Fatalf("first Compact: %v", err)
	}
	first := scoredHits(t, idx, query, 10000)
	firstCard, firstDocs := corpusStats(t, idx, "_all")

	if err := idx.Compact(context.Background()); err != nil {
		t.Fatalf("second Compact: %v", err)
	}
	secondCard, secondDocs := corpusStats(t, idx, "_all")
	if secondCard != firstCard || secondDocs != firstDocs {
		t.Errorf("second compaction changed the corpus statistics: %d/%d -> %d/%d",
			firstCard, firstDocs, secondCard, secondDocs)
	}
	if diff := diffScores(scoredHits(t, idx, query, 10000), first); len(diff) > 0 {
		t.Errorf("second compaction changed the scores:\n\t%s", strings.Join(diff, "\n\t"))
	}
}

// TestNoCompactSeparatesPolicyFromCapability: indexes.bm25.no_compact says a
// finished pass leaves the layout alone, which is what a bulk loader wants —
// and it must not also mean the index can never be settled, because that same
// loader asks for one compaction when it is done (see service.CompactIndexes).
// AutoCompact carries the policy; Compact does the work either way.
func TestNoCompactSeparatesPolicyFromCapability(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75, NoCompact: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()

	if idx.AutoCompact() {
		t.Error("AutoCompact() = true with NoCompact set: a pass would settle the layout anyway")
	}

	for start, files := 0, corpusFiles(200); start < len(files); start += 16 {
		batch := files[start:min(start+16, len(files))]
		if _, err := idx.Index(context.Background(), &indexing.IndexRequest{RepoID: "r1", Files: batch}); err != nil {
			t.Fatalf("Index: %v", err)
		}
	}

	// An explicit compaction is a command, not a preference: it settles the
	// layout, which is what makes the scores of the run that follows repeatable.
	if err := idx.Compact(context.Background()); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	scoredHits(t, idx, "which entity maps to the visits table", 3)
}

// Compact must survive being called from several passes at once: two
// repositories can finish within the same instant, and bleve refuses a second
// force merge while one is running.
func TestCompactConcurrent(t *testing.T) {
	idx := buildInWindows(t, corpusFiles(200), 16)

	errs := make(chan error, 4)
	for w := 0; w < 4; w++ {
		go func() { errs <- idx.Compact(context.Background()) }()
	}
	for w := 0; w < 4; w++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Compact: %v", err)
		}
	}
	scoredHits(t, idx, "appointment schedule reminder", 3)
}

// The service reaches Compact through this interface; a build that drops it
// silently stops compacting.
func TestIndexerIsACompactor(t *testing.T) {
	idx, err := New(&Config{Path: t.TempDir(), K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	if _, ok := any(idx).(indexing.Compactor); !ok {
		t.Fatal("*Indexer no longer implements indexing.Compactor")
	}
}

// The two ways an index gets to the same content have to agree, because the
// eval harness reaches it both ways: a fresh build on a clean directory, and a
// forced pass over an index that already holds the repository. The second
// leaves the rewritten chunks in new segments and the ones they replaced
// behind as deletes, which is a layout a fresh build never has.
func TestCompactedReindexMatchesAFreshBuild(t *testing.T) {
	files := corpusFiles(400)
	const query = "which entity maps to the visits table"
	const everything = 10000

	dir := t.TempDir()
	fresh := buildInWindowsAt(t, dir, files, 64)
	if err := fresh.Compact(context.Background()); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	wantCard, wantDocs := corpusStats(t, fresh, "_all")
	want := scoredHits(t, fresh, query, everything)

	// Reopening must not disturb it: the layout is on disk, and a server
	// restart between indexing and querying is the normal case.
	if err := fresh.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := New(&Config{Path: dir, K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if card, docs := corpusStats(t, reopened, "_all"); card != wantCard || docs != wantDocs {
		t.Errorf("reopening changed the corpus statistics: %d/%d, want %d/%d", card, docs, wantCard, wantDocs)
	}
	if diff := diffScores(scoredHits(t, reopened, query, everything), want); len(diff) > 0 {
		t.Errorf("reopening changed the scores:\n\t%s", strings.Join(diff, "\n\t"))
	}

	// Index the same content again over it, as a forced pass does.
	for start := 0; start < len(files); start += 64 {
		batch := make([]*indexing.FileToIndex, 0, 64)
		for _, f := range files[start:min(start+64, len(files))] {
			cp := *f
			batch = append(batch, &cp)
		}
		if _, err := reopened.Index(context.Background(), &indexing.IndexRequest{RepoID: "r1", Files: batch}); err != nil {
			t.Fatalf("reindex: %v", err)
		}
	}
	if err := reopened.Compact(context.Background()); err != nil {
		t.Fatalf("Compact after reindex: %v", err)
	}

	card, docs := corpusStats(t, reopened, "_all")
	if docs != wantDocs {
		t.Errorf("re-indexing the same files changed the document count: %d, want %d", docs, wantDocs)
	}
	if card != wantCard {
		t.Errorf("compacted cardinality after re-index = %d, want %d — the chunks the re-index replaced "+
			"are still contributing their vocabulary", card, wantCard)
	}
	if diff := diffScores(scoredHits(t, reopened, query, everything), want); len(diff) > 0 {
		t.Errorf("a re-index scores the same sources differently from a fresh build:\n\t%s", strings.Join(diff, "\n\t"))
	}
}

// A compaction of an index that is already one segment must return without
// waiting on scorch's background loops. Their epochs advance only when the
// merger and persister run, and on an idle, freshly reopened index they never
// do — the old path stalled admin/compact for tens of minutes against a cycle
// that was never coming, over an index compacted hours earlier.
func TestCompactOfCompactedReopenedIndexReturnsImmediately(t *testing.T) {
	dir := t.TempDir()
	idx, err := New(&Config{Path: dir, K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for start, files := 0, corpusFiles(200); start < len(files); start += 16 {
		batch := files[start:min(start+16, len(files))]
		if _, err := idx.Index(context.Background(), &indexing.IndexRequest{RepoID: "r1", Files: batch}); err != nil {
			t.Fatalf("Index: %v", err)
		}
	}
	if err := idx.Compact(context.Background()); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	re, err := New(&Config{Path: dir, K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer re.Close()

	start := time.Now()
	if err := re.Compact(context.Background()); err != nil {
		t.Fatalf("Compact after reopen: %v", err)
	}
	if took := time.Since(start); took > time.Minute {
		t.Fatalf("Compact on a cold compacted index took %v; it must answer from the segment count, not wait for background cycles", took)
	}
}
