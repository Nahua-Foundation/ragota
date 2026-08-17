package enrich

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseChoice(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"1", 1, false},
		{" 2 \n", 2, false},
		{"The answer is 3.", 3, false},
		{"-1", -1, false},
		{"I think candidate 0) is right", 0, false},
		{"no idea", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := parseChoice(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("parseChoice(%q) = %d, err %v; want %d, wantErr %v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	want := `{"services":[]}`
	for _, in := range []string{
		want,
		"```json\n" + want + "\n```",
		"Here you go:\n" + want + "\nHope that helps!",
	} {
		if got := extractJSON(in); got != want {
			t.Errorf("extractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildReconOverviewCaps(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(strings.Repeat("r", 5000)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Directories below depth 3 must not appear.
	deep := filepath.Join(dir, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "go.mod"), []byte("module a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := buildReconOverview(dir, "caps")
	if len(out) > reconOverviewBytes {
		t.Errorf("overview length = %d, want <= %d", len(out), reconOverviewBytes)
	}
	if !strings.Contains(out, "a/go.mod") {
		t.Errorf("overview misses manifest a/go.mod:\n%s", out)
	}
	if strings.Contains(out, "a/b/c/d") {
		t.Errorf("overview lists a directory beyond depth %d:\n%s", reconMaxDepth, out)
	}
}
