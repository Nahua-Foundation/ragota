package zapverify

// Regression test for the segment corruption behind "memUvarintReader
// overflow" / index-out-of-range panics in BM25 search (zapx v17.1.2,
// intcoder.go Write).
//
// The writer's chunkedIntCoder.Write converts its chunk-length table to
// end-offsets in place, then — because every zap v17 writer is a *FileWriter —
// walks the chunks through the file callback, skipping empty chunks. The
// skipped slots keep the end-offset the first pass left behind, and the second
// lengths-to-offsets pass then treats those offsets as lengths. Any term whose
// postings have an empty chunk in the middle of its doc range gets an offset
// table that overruns its data, and the postings reader walks off into foreign
// bytes — decoding garbage varints exactly the way production searches did.
//
// The layout below reproduces the production shape deterministically: a term
// present in an early doc range, absent for a full chunk, present again. That
// is what a repo_id term looks like in a merged segment whose middle segments
// belong to another repository — which is why whole repositories went dark at
// once while the rest of the index stayed fine.

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	index "github.com/blevesearch/bleve_index_api"
	segapi "github.com/blevesearch/scorch_segment_api/v2"
	zap "github.com/blevesearch/zapx/v17"
)

// fakeField is a pre-analyzed field: Analyze is a no-op because the token
// frequencies are handed in. That keeps the test hermetic — no analyzer
// registry, no mapping, just the exact postings layout the writer must encode.
type fakeField struct {
	name    string
	value   []byte
	freqs   index.TokenFrequencies
	length  int
	options index.FieldIndexingOptions
}

func newFakeField(name, token string) *fakeField {
	tf := &index.TokenFreq{Term: []byte(token)}
	tf.SetFrequency(1)
	return &fakeField{
		name:    name,
		value:   []byte(token),
		freqs:   index.TokenFrequencies{token: tf},
		length:  1,
		options: index.IndexField,
	}
}

// newFakeIDField is the "_id" stored field zapx requires at fieldID 0.
func newFakeIDField(id string) *fakeField {
	f := newFakeField("_id", id)
	f.options = index.IndexField | index.StoreField
	return f
}

func (f *fakeField) Name() string             { return f.name }
func (f *fakeField) Value() []byte            { return f.value }
func (f *fakeField) ArrayPositions() []uint64 { return nil }
func (f *fakeField) EncodedFieldType() byte   { return 't' }
func (f *fakeField) Analyze()                 {}
func (f *fakeField) Options() index.FieldIndexingOptions {
	return f.options
}
func (f *fakeField) AnalyzedLength() int                              { return f.length }
func (f *fakeField) AnalyzedTokenFrequencies() index.TokenFrequencies { return f.freqs }
func (f *fakeField) NumPlainTextBytes() uint64                        { return uint64(len(f.value)) }

type fakeDoc struct {
	id     string
	fields []index.Field
}

func (d *fakeDoc) ID() string { return d.id }
func (d *fakeDoc) Size() int  { return 0 }
func (d *fakeDoc) VisitFields(v index.FieldVisitor) {
	for _, f := range d.fields {
		v(f)
	}
}
func (d *fakeDoc) VisitComposite(index.CompositeFieldVisitor) {}
func (d *fakeDoc) HasComposite() bool                         { return false }
func (d *fakeDoc) NumPlainTextBytes() uint64                  { return 0 }
func (d *fakeDoc) AddIDField()                                {}
func (d *fakeDoc) StoredFieldsBytes() uint64                  { return 0 }
func (d *fakeDoc) Indexed() bool                              { return true }

// buildSegment builds one in-memory segment of n docs whose "body" holds token.
func buildSegment(t *testing.T, n int, token, idPrefix string) *zap.SegmentBase {
	t.Helper()
	docs := make([]index.Document, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%06d", idPrefix, i)
		docs = append(docs, &fakeDoc{
			id:     id,
			fields: []index.Field{newFakeIDField(id), newFakeField("body", token)},
		})
	}
	seg, _, err := (&zap.ZapPlugin{}).New(docs)
	if err != nil {
		t.Fatalf("build segment: %v", err)
	}
	return seg.(*zap.SegmentBase)
}

// TestMergedTermWithEmptyMiddleChunkDecodes builds the minimal merged segment
// whose "needle" term has an empty middle chunk and asserts every posting
// decodes.
//
// Sizing: 1100 needle docs + 1100 filler docs + 1100 needle docs. Cardinality
// 2200 makes chunkMode 1026 pick numChunks = 2200/1024+1 = 3, so chunkSize =
// 3300/3 = 1100 and chunk 1 (docs 1100..2199) holds no needle postings — the
// exact shape of a repo_id term in a segment merged across two repositories.
func TestMergedTermWithEmptyMiddleChunkDecodes(t *testing.T) {
	const block = 1100

	a := buildSegment(t, block, "needle", "a")
	b := buildSegment(t, block, "filler", "b")
	c := buildSegment(t, block, "needle", "c")

	path := filepath.Join(t.TempDir(), "merged.zap")
	drops := []*roaring.Bitmap{nil, nil, nil}
	_, _, err := (&zap.ZapPlugin{}).Merge(
		[]segapi.Segment{a, b, c}, drops, path, nil, nil)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	report := Segment(path)
	if !report.CRCOK {
		t.Fatalf("merged segment CRC mismatch: stored=%08x computed=%08x",
			report.StoredCRC, report.ComputedCRC)
	}
	for _, f := range report.Failures {
		t.Errorf("undecodable postings: field=%q term=%q offset=%d: %s",
			f.Field, f.Term, f.PostOff, f.Err)
	}
	if t.Failed() {
		t.Logf("segment report: %s", report.String())
	}
}

// TestFlushedTermWithEmptyMiddleChunkDecodes exercises the same table bug on
// the flush path (zapx New + persist, no merge): one batch whose term skips a
// full chunk in the middle of the doc range. Production hits this less often
// than the merge shape, but the writer code is the same.
func TestFlushedTermWithEmptyMiddleChunkDecodes(t *testing.T) {
	const block = 1100
	docs := make([]index.Document, 0, 3*block)
	for i := 0; i < block; i++ {
		id := fmt.Sprintf("a-%06d", i)
		docs = append(docs, &fakeDoc{
			id:     id,
			fields: []index.Field{newFakeIDField(id), newFakeField("body", "needle")},
		})
	}
	for i := 0; i < block; i++ {
		id := fmt.Sprintf("b-%06d", i)
		docs = append(docs, &fakeDoc{
			id:     id,
			fields: []index.Field{newFakeIDField(id), newFakeField("body", "filler")},
		})
	}
	for i := 0; i < block; i++ {
		id := fmt.Sprintf("c-%06d", i)
		docs = append(docs, &fakeDoc{
			id:     id,
			fields: []index.Field{newFakeIDField(id), newFakeField("body", "needle")},
		})
	}

	seg, _, err := (&zap.ZapPlugin{}).New(docs)
	if err != nil {
		t.Fatalf("build segment: %v", err)
	}
	path := filepath.Join(t.TempDir(), "flushed.zap")
	if err := zap.PersistSegmentBase(seg.(*zap.SegmentBase), path); err != nil {
		t.Fatalf("persist: %v", err)
	}

	report := Segment(path)
	if !report.CRCOK {
		t.Fatalf("flushed segment CRC mismatch: stored=%08x computed=%08x",
			report.StoredCRC, report.ComputedCRC)
	}
	for _, f := range report.Failures {
		t.Errorf("undecodable postings: field=%q term=%q offset=%d: %s",
			f.Field, f.Term, f.PostOff, f.Err)
	}
	if t.Failed() {
		t.Logf("segment report: %s", report.String())
	}
}
