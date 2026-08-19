// Package zapverify validates zap segment files offline: it checks the footer
// CRC against the file bytes and fully decodes every term's postings (freq,
// norm and locations) in every field, reporting exactly which (segment, field,
// term) fails and where.
//
// It exists because a damaged postings chunk table detonates only when a
// search happens to walk past the poisoned chunk boundary — an index can look
// healthy for every query asked of it and still be corrupt. Walking every
// posting is the only check that answers "is this index readable" rather than
// "were today's queries lucky".
package zapverify

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blevesearch/vellum"
	zap "github.com/blevesearch/zapx/v17"
)

// TermFailure is one term whose postings do not decode.
type TermFailure struct {
	Field   string
	Term    string
	PostOff uint64 // FST value: the postings offset in the segment
	Err     string
}

// SegmentReport is the verification result for one .zap file.
type SegmentReport struct {
	Path        string
	Size        int64
	Docs        uint64
	Fields      int
	Terms       int
	CRCOK       bool
	StoredCRC   uint32
	ComputedCRC uint32
	OpenErr     string
	Failures    []TermFailure
}

// OK reports whether the segment verified clean.
func (r *SegmentReport) OK() bool {
	return r.OpenErr == "" && r.CRCOK && len(r.Failures) == 0
}

// String renders a one-line summary.
func (r *SegmentReport) String() string {
	status := "OK"
	if !r.OK() {
		status = "CORRUPT"
	}
	return fmt.Sprintf("%s: %s size=%d docs=%d fields=%d terms=%d badTerms=%d crcMatch=%v",
		filepath.Base(r.Path), status, r.Size, r.Docs, r.Fields, r.Terms, len(r.Failures), r.CRCOK)
}

// Store verifies every .zap file in a scorch store directory.
func Store(dir string) ([]*SegmentReport, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read store dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zap") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)

	reports := make([]*SegmentReport, 0, len(files))
	for _, f := range files {
		reports = append(reports, Segment(f))
	}
	return reports, nil
}

// Segment verifies one .zap file.
func Segment(path string) (rv *SegmentReport) {
	rv = &SegmentReport{Path: path}

	// checkField and checkChunkTables index into the segment by offsets read
	// from the segment itself; on a corrupt segment those offsets can slice out
	// of bounds. Recover so a damaged segment is reported as unreadable rather
	// than crashing the caller — the same guarantee checkPostings already gives
	// per term, extended to the whole walk.
	defer func() {
		if r := recover(); r != nil && rv.OpenErr == "" {
			rv.OpenErr = fmt.Sprintf("panic reading segment: %v", r)
		}
	}()

	raw, err := os.ReadFile(path)
	if err != nil {
		rv.OpenErr = err.Error()
		return rv
	}
	rv.Size = int64(len(raw))
	if len(raw) < 4 {
		rv.OpenErr = fmt.Sprintf("file too short: %d bytes", len(raw))
		return rv
	}
	rv.StoredCRC = binary.BigEndian.Uint32(raw[len(raw)-4:])
	rv.ComputedCRC = crc32.ChecksumIEEE(raw[:len(raw)-4])
	rv.CRCOK = rv.StoredCRC == rv.ComputedCRC

	segIface, err := (&zap.ZapPlugin{}).Open(path)
	if err != nil {
		rv.OpenErr = err.Error()
		return rv
	}
	seg := segIface.(*zap.Segment)
	defer seg.Close()

	rv.Docs = seg.NumDocs()
	fields := seg.Fields()
	sort.Strings(fields)
	rv.Fields = len(fields)

	for _, field := range fields {
		rv.Terms += checkField(seg, field, &rv.Failures)
	}
	return rv
}

// checkField walks one field's dictionary and postings; returns the term count.
func checkField(seg *zap.Segment, field string, failures *[]TermFailure) int {
	data := seg.Data()

	addr, err := seg.DictAddr(field)
	if err != nil {
		*failures = append(*failures, TermFailure{Field: field, Err: fmt.Sprintf("DictAddr: %v", err)})
		return 0
	}
	vellumLen, read := binary.Uvarint(data[addr : addr+binary.MaxVarintLen64])
	fstBytes := data[addr+uint64(read) : addr+uint64(read)+vellumLen]
	fst, err := vellum.Load(fstBytes)
	if err != nil {
		*failures = append(*failures, TermFailure{Field: field, Err: fmt.Sprintf("vellum.Load: %v", err)})
		return 0
	}

	dictIface, err := seg.Dictionary(field)
	if err != nil {
		*failures = append(*failures, TermFailure{Field: field, Err: fmt.Sprintf("Dictionary: %v", err)})
		return 0
	}
	dict := dictIface.(*zap.Dictionary)

	terms := 0
	itr, err := fst.Iterator(nil, nil)
	for err == nil {
		currTerm, currVal := itr.Current()
		terms++
		// 1-hit encoded terms carry docNum and norm in the FST value itself;
		// there are no postings bytes to decode.
		if currVal&zap.FSTValEncodingMask != zap.FSTValEncoding1Hit {
			term := append([]byte(nil), currTerm...)
			if ferr := checkChunkTables(data, currVal); ferr != "" {
				*failures = append(*failures, TermFailure{Field: field, Term: string(term), PostOff: currVal, Err: ferr})
			} else if ferr := checkPostings(dict, term); ferr != "" {
				*failures = append(*failures, TermFailure{Field: field, Term: string(term), PostOff: currVal, Err: ferr})
			}
		}
		err = itr.Next()
	}
	if err != nil && err != vellum.ErrIteratorDone {
		*failures = append(*failures, TermFailure{Field: field, Err: fmt.Sprintf("fst iterate: %v", err)})
	}
	return terms
}

// checkChunkTables validates the structural invariant a correct writer always
// maintains: a term's freq and loc chunk-offset tables, plus the data they
// describe, exactly fill the byte range up to the next recorded structure
// (freq runs to the loc table when there is one, otherwise to the postings
// head; loc runs to the postings head). A table whose claimed extent
// disagrees with its region sends readers into foreign bytes — that mismatch
// is the on-disk corruption itself, whether or not a walk through the
// postings happens to trip over it, since the varint reader silently returns
// zero once a mis-sliced chunk runs out.
func checkChunkTables(data []byte, postingsOffset uint64) string {
	if postingsOffset >= uint64(len(data)) {
		return fmt.Sprintf("postings offset %d beyond segment (%d)", postingsOffset, len(data))
	}
	var n uint64
	freqOff, read := binary.Uvarint(data[postingsOffset+n : postingsOffset+n+binary.MaxVarintLen64])
	n += uint64(read)
	locOff, _ := binary.Uvarint(data[postingsOffset+n : postingsOffset+n+binary.MaxVarintLen64])

	if freqOff != 0 { // termNotEncoded
		end := postingsOffset
		if locOff != 0 {
			end = locOff
		}
		if msg := checkChunkTable(data, freqOff, end, "freq"); msg != "" {
			return msg
		}
	}
	if locOff != 0 {
		if msg := checkChunkTable(data, locOff, postingsOffset, "loc"); msg != "" {
			return msg
		}
	}
	return ""
}

// checkChunkTable verifies one chunk table starting at tableOff whose data
// must end exactly at regionEnd.
func checkChunkTable(data []byte, tableOff, regionEnd uint64, label string) string {
	if tableOff >= uint64(len(data)) || regionEnd > uint64(len(data)) || tableOff >= regionEnd {
		return fmt.Sprintf("%s table bounds invalid: table=%d regionEnd=%d segment=%d",
			label, tableOff, regionEnd, len(data))
	}
	off := tableOff
	numChunks, read := binary.Uvarint(data[off : off+binary.MaxVarintLen64])
	if read <= 0 {
		return fmt.Sprintf("%s table: bad numChunks varint", label)
	}
	off += uint64(read)

	var last, prev uint64
	for i := uint64(0); i < numChunks; i++ {
		if off >= regionEnd {
			return fmt.Sprintf("%s table: offsets overrun region (chunk %d of %d)", label, i, numChunks)
		}
		co, r := binary.Uvarint(data[off : off+binary.MaxVarintLen64])
		if r <= 0 {
			return fmt.Sprintf("%s table: bad offset varint (chunk %d)", label, i)
		}
		if co < prev {
			return fmt.Sprintf("%s table: offsets not monotonic (chunk %d: %d < %d)", label, i, co, prev)
		}
		prev = co
		last = co
		off += uint64(r)
	}

	if got, want := off+last, regionEnd; got != want {
		return fmt.Sprintf("%s chunk table inconsistent: header+data ends at %d, region ends at %d (off by %d)",
			label, got, want, int64(got)-int64(want))
	}
	return ""
}

// checkPostings fully decodes one term's postings; returns "" when clean.
func checkPostings(dict *zap.Dictionary, term []byte) (failure string) {
	defer func() {
		if r := recover(); r != nil {
			failure = fmt.Sprintf("panic: %v", r)
		}
	}()

	pl, err := dict.PostingsList(term, nil, nil)
	if err != nil {
		return fmt.Sprintf("PostingsList: %v", err)
	}
	it := pl.Iterator(true, true, true, nil)
	n := 0
	for {
		p, err := it.Next()
		if err != nil {
			return fmt.Sprintf("posting %d Next: %v", n, err)
		}
		if p == nil {
			return ""
		}
		_ = p.Number()
		_ = p.Frequency()
		_ = p.Norm()
		for _, loc := range p.Locations() {
			_ = loc.Pos()
			_ = loc.Start()
			_ = loc.End()
		}
		n++
	}
}
