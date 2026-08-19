// Command zapcheck validates every zap segment file in a bleve scorch store
// directory offline: footer CRC against the file bytes, the structural
// consistency of every term's chunk-offset tables, and a full decode of every
// posting list. It reports exactly which (segment, field, term) is unreadable.
//
// It exists to turn "searches intermittently hit memUvarintReader overflow"
// into a byte-level fact about one segment produced by one writer. Point it at
// a COPY of the index; it opens segments read-only and never writes, but
// keeping investigation material away from live directories is the rule the
// probe tool already established.
//
// Exit status: 0 when every segment verifies clean, 1 otherwise.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Nahua-Foundation/ragota/tools/zapcheck/zapverify"
)

func main() {
	store := flag.String("store", "", "path to the scorch store directory (contains *.zap)")
	one := flag.String("segment", "", "check only this .zap file")
	maxReport := flag.Int("max-report", 8, "failures to report in detail per segment")
	flag.Parse()

	var reports []*zapverify.SegmentReport
	switch {
	case *one != "":
		reports = []*zapverify.SegmentReport{zapverify.Segment(*one)}
	case *store != "":
		var err error
		reports, err = zapverify.Store(*store)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: zapcheck -store <dir> | -segment <file.zap>")
		os.Exit(2)
	}

	exit := 0
	for _, r := range reports {
		fmt.Println(r.String())
		if r.OpenErr != "" {
			fmt.Printf("  open error: %s\n", r.OpenErr)
		}
		for i, f := range r.Failures {
			if i >= *maxReport {
				fmt.Printf("  ... and %d more failing terms\n", len(r.Failures)-*maxReport)
				break
			}
			fmt.Printf("  field=%q term=%q postingsOffset=%d (0x%x): %s\n",
				f.Field, f.Term, f.PostOff, f.PostOff, f.Err)
		}
		if !r.OK() {
			exit = 1
		}
	}
	os.Exit(exit)
}
