package ast

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// memStorage is a minimal in-memory store.Storage implementation covering
// only the methods used by Indexer.Index. All other methods panic via the
// embedded nil interface.
type memStorage struct {
	store.Storage

	mu     sync.Mutex
	units  []*domain.ASTUnit
	edges  []*domain.Edge
	nextID int

	// calls counts the write calls per method: one call is one transaction in
	// both real backends, and the transaction count is the whole point of
	// batching the store stage.
	calls map[string]int

	// failUnitsFor makes BatchStoreASTUnits reject any batch containing this
	// file, to exercise the per-file retry the window store falls back on.
	failUnitsFor string
	// failDeleteFor makes DeleteASTUnitsByFiles reject any batch containing
	// this file, to exercise the same retry on the delete side.
	failDeleteFor string
}

func (m *memStorage) record(method string) {
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	m.calls[method]++
}

func (m *memStorage) DeleteASTUnitsByFile(_ context.Context, repoID, filePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("DeleteASTUnitsByFile")
	kept := m.units[:0]
	for _, u := range m.units {
		if !(u.RepoID == repoID && u.FilePath == filePath) {
			kept = append(kept, u)
		}
	}
	m.units = kept
	return nil
}

func (m *memStorage) DeleteEdgesByFile(_ context.Context, repoID, filePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("DeleteEdgesByFile")
	kept := m.edges[:0]
	for _, e := range m.edges {
		if !(e.RepoID == repoID && e.FilePath == filePath) {
			kept = append(kept, e)
		}
	}
	m.edges = kept
	return nil
}

func (m *memStorage) DeleteASTUnitsByFiles(_ context.Context, repoID string, paths []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("DeleteASTUnitsByFiles")
	if m.failDeleteFor != "" && slices.Contains(paths, m.failDeleteFor) {
		return fmt.Errorf("rejected %s", m.failDeleteFor)
	}
	want := make(map[string]bool, len(paths))
	for _, p := range paths {
		want[p] = true
	}
	kept := m.units[:0]
	for _, u := range m.units {
		if !(u.RepoID == repoID && want[u.FilePath]) {
			kept = append(kept, u)
			continue
		}
		// The real backends unresolve every edge pointing at a deleted unit.
		for _, e := range m.edges {
			if e.DstID == u.ID {
				e.DstID, e.DstRepoID = "", ""
			}
		}
	}
	m.units = kept
	return nil
}

func (m *memStorage) DeleteEdgesByFiles(_ context.Context, repoID string, paths []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("DeleteEdgesByFiles")
	want := make(map[string]bool, len(paths))
	for _, p := range paths {
		want[p] = true
	}
	kept := m.edges[:0]
	for _, e := range m.edges {
		if !(e.RepoID == repoID && want[e.FilePath]) {
			kept = append(kept, e)
		}
	}
	m.edges = kept
	return nil
}

func (m *memStorage) BatchStoreASTUnits(_ context.Context, units []*domain.ASTUnit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("BatchStoreASTUnits")
	for _, u := range units {
		if m.failUnitsFor != "" && u.FilePath == m.failUnitsFor {
			return fmt.Errorf("rejected %s", u.FilePath)
		}
	}
	for _, u := range units {
		if u.ID == "" {
			m.nextID++
			u.ID = fmt.Sprintf("%d", m.nextID)
		}
		m.units = append(m.units, u)
	}
	return nil
}

func (m *memStorage) BatchStoreEdges(_ context.Context, edges []*domain.Edge) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("BatchStoreEdges")
	m.edges = append(m.edges, edges...)
	return nil
}

// syntheticGoFiles builds n distinct Go files with types, functions and calls
// so that parsing yields both units and edges.
func syntheticGoFiles(n int) []*index.FileToIndex {
	files := make([]*index.FileToIndex, 0, n)
	for i := 0; i < n; i++ {
		content := fmt.Sprintf(
			"package p%d\n\n"+
				"type Thing%d struct{ Name string }\n\n"+
				"func (t *Thing%d) Describe%d() string { return t.Name }\n\n"+
				"func Helper%d() int { return %d }\n\n"+
				"func Main%d() int {\n\tt := &Thing%d{}\n\t_ = t.Describe%d()\n\treturn Helper%d()\n}\n",
			i, i, i, i, i, i, i, i, i, i,
		)
		files = append(files, &index.FileToIndex{
			Path:     fmt.Sprintf("pkg%02d/file%02d.go", i, i),
			Language: "go",
			Content:  []byte(content),
		})
	}
	return files
}

func runASTIndex(t *testing.T, workers, nFiles int) (*index.IndexResult, *memStorage) {
	t.Helper()

	st := &memStorage{}
	idx := New(&Config{Storage: st, Workers: workers})
	idx.RegisterParser(NewTreeSitterParser("go"))

	res, err := idx.Index(context.Background(), &index.IndexRequest{
		RepoID:   "repo-parallel",
		RepoName: "repo-parallel",
		Files:    syntheticGoFiles(nFiles),
	})
	if err != nil {
		t.Fatalf("Index(workers=%d): %v", workers, err)
	}
	return res, st
}

func unitKeys(st *memStorage) map[string]int {
	keys := make(map[string]int)
	for _, u := range st.units {
		keys[u.FilePath+"|"+u.Kind+"|"+u.Name]++
	}
	return keys
}

func edgeKeys(st *memStorage) map[string]int {
	keys := make(map[string]int)
	for _, e := range st.edges {
		keys[e.Kind+"|"+e.DstName]++
	}
	return keys
}

// TestIndexParallelMatchesSequential runs Index with Workers=1 and Workers=4
// on the same synthetic input and verifies the results are equivalent:
// identical counters and identical multisets of stored units and edges.
// Run with -race to also verify the parse stage is data-race free.
func TestIndexParallelMatchesSequential(t *testing.T) {
	const nFiles = 20

	seqRes, seqSt := runASTIndex(t, 1, nFiles)
	parRes, parSt := runASTIndex(t, 4, nFiles)

	if seqRes.FilesIndexed != nFiles {
		t.Fatalf("sequential FilesIndexed = %d, want %d", seqRes.FilesIndexed, nFiles)
	}
	if parRes.FilesIndexed != seqRes.FilesIndexed {
		t.Errorf("FilesIndexed: parallel = %d, sequential = %d", parRes.FilesIndexed, seqRes.FilesIndexed)
	}
	if parRes.FilesFailed != seqRes.FilesFailed {
		t.Errorf("FilesFailed: parallel = %d, sequential = %d", parRes.FilesFailed, seqRes.FilesFailed)
	}
	if parRes.FilesSkipped != seqRes.FilesSkipped {
		t.Errorf("FilesSkipped: parallel = %d, sequential = %d", parRes.FilesSkipped, seqRes.FilesSkipped)
	}
	if len(parRes.Errors) != len(seqRes.Errors) {
		t.Errorf("Errors: parallel = %v, sequential = %v", parRes.Errors, seqRes.Errors)
	}

	seqUnits, parUnits := unitKeys(seqSt), unitKeys(parSt)
	if len(seqUnits) == 0 {
		t.Fatal("sequential run stored no units")
	}
	if len(parUnits) != len(seqUnits) {
		t.Errorf("distinct unit keys: parallel = %d, sequential = %d", len(parUnits), len(seqUnits))
	}
	for k, n := range seqUnits {
		if parUnits[k] != n {
			t.Errorf("unit %q: parallel count = %d, sequential count = %d", k, parUnits[k], n)
		}
	}
	for k := range parUnits {
		if _, ok := seqUnits[k]; !ok {
			t.Errorf("unit %q present only in parallel run", k)
		}
	}

	seqEdges, parEdges := edgeKeys(seqSt), edgeKeys(parSt)
	if len(seqEdges) == 0 {
		t.Fatal("sequential run stored no edges")
	}
	for k, n := range seqEdges {
		if parEdges[k] != n {
			t.Errorf("edge %q: parallel count = %d, sequential count = %d", k, parEdges[k], n)
		}
	}
	for k := range parEdges {
		if _, ok := seqEdges[k]; !ok {
			t.Errorf("edge %q present only in parallel run", k)
		}
	}
}

func TestParseWorkers(t *testing.T) {
	want := runtime.NumCPU()
	if want > maxParseWorkers {
		want = maxParseWorkers
	}
	if got := parseWorkers(0); got != want {
		t.Errorf("parseWorkers(0) = %d, want %d", got, want)
	}
	if got := parseWorkers(4); got != 4 {
		t.Errorf("parseWorkers(4) = %d, want 4", got)
	}
	if got := parseWorkers(100); got != maxParseWorkers {
		t.Errorf("parseWorkers(100) = %d, want %d", got, maxParseWorkers)
	}
	if got := parseWorkers(-3); got != want {
		t.Errorf("parseWorkers(-3) = %d, want %d", got, want)
	}
}
