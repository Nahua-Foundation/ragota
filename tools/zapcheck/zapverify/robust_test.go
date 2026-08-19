package zapverify

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSegmentDoesNotPanicOnCorruptInput pins the recover added to Segment: a
// file that is not a readable zap segment must come back as an OpenErr, never a
// panic that takes the caller (tools/zapcheck, an ops cron) down.
func TestSegmentDoesNotPanicOnCorruptInput(t *testing.T) {
	cases := map[string][]byte{
		"empty":     {},
		"tooShort":  {0x01, 0x02},
		"garbage":   []byte("this is not a zap segment, but it is long enough"),
		"zeroed":    make([]byte, 512),
		"truncated": append([]byte("\x0c\x00\x00zapx"), make([]byte, 40)...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "seg.zap")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			rv := Segment(path) // must not panic
			if rv == nil {
				t.Fatal("Segment returned nil")
			}
			if rv.OpenErr == "" {
				t.Errorf("expected a non-empty OpenErr for a corrupt segment, got a clean report")
			}
		})
	}
}
