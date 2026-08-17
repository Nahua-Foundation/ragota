package indexing

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHitJSONIsSnakeCase pins the wire format: the search response used to mix
// Go field names (RepoID, FilePath) with snake_case siblings.
func TestHitJSONIsSnakeCase(t *testing.T) {
	hit := &Hit{
		RepoID:   "r1",
		FilePath: "src/a.go",
		Path:     "src/a.go",
		Line:     3,
		EndLine:  9,
		Symbol:   "Add",
		Kind:     "function",
		Language: "go",
		Score:    0.5,
		Snippet:  "func Add",
		Reason:   "semantic+keyword",
	}

	raw, err := json.Marshal(hit)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for _, key := range []string{"repo_id", "file_path", "path", "line", "end_line", "symbol", "kind", "language", "score", "snippet", "reason"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing %q in %s", key, raw)
		}
	}
	for key := range got {
		if strings.ToLower(key) != key {
			t.Errorf("field %q is not snake_case", key)
		}
	}
}

func TestHitOverlaps(t *testing.T) {
	tests := []struct {
		name string
		a, b *Hit
		want bool
	}{
		{
			name: "window contains card",
			a:    &Hit{RepoID: "r1", FilePath: "a.go", Line: 1, EndLine: 60},
			b:    &Hit{RepoID: "r1", FilePath: "a.go", Line: 55, EndLine: 58},
			want: true,
		},
		{
			name: "adjacent but disjoint",
			a:    &Hit{RepoID: "r1", FilePath: "a.go", Line: 1, EndLine: 60},
			b:    &Hit{RepoID: "r1", FilePath: "a.go", Line: 61, EndLine: 90},
			want: false,
		},
		{
			name: "different file",
			a:    &Hit{RepoID: "r1", FilePath: "a.go", Line: 1, EndLine: 60},
			b:    &Hit{RepoID: "r1", FilePath: "b.go", Line: 1, EndLine: 60},
			want: false,
		},
		{
			name: "different repo",
			a:    &Hit{RepoID: "r1", FilePath: "a.go", Line: 1, EndLine: 60},
			b:    &Hit{RepoID: "r2", FilePath: "a.go", Line: 1, EndLine: 60},
			want: false,
		},
		{
			name: "missing end line collapses to the start",
			a:    &Hit{RepoID: "r1", FilePath: "a.go", Line: 5},
			b:    &Hit{RepoID: "r1", FilePath: "a.go", Line: 1, EndLine: 60},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Overlaps(tt.b); got != tt.want {
				t.Errorf("a.Overlaps(b) = %v, want %v", got, tt.want)
			}
			if got := tt.b.Overlaps(tt.a); got != tt.want {
				t.Errorf("b.Overlaps(a) = %v, want %v (must be symmetric)", got, tt.want)
			}
		})
	}
}

func TestParseFilters(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]interface{}
		want Filters
	}{
		{name: "nil", raw: nil, want: Filters{}},
		{
			name: "singular and plural are equivalent",
			raw:  map[string]interface{}{"language": "go", "kinds": []string{"function", "method"}},
			want: Filters{Languages: []string{"go"}, Kinds: []string{"function", "method"}},
		},
		{
			name: "list of any",
			raw:  map[string]interface{}{"languages": []interface{}{"go", " python "}},
			want: Filters{Languages: []string{"go", "python"}},
		},
		{
			name: "path and path_prefix are the same key",
			raw:  map[string]interface{}{"path": "internal/"},
			want: Filters{PathPrefix: "internal/"},
		},
		{
			name: "unknown keys and unusable values are ignored",
			raw:  map[string]interface{}{"colour": "red", "language": 42},
			want: Filters{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseFilters(tt.raw)
			if strings.Join(got.Languages, ",") != strings.Join(tt.want.Languages, ",") ||
				strings.Join(got.Kinds, ",") != strings.Join(tt.want.Kinds, ",") ||
				got.PathPrefix != tt.want.PathPrefix {
				t.Errorf("ParseFilters() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFiltersMatch(t *testing.T) {
	f := Filters{Languages: []string{"go"}, Kinds: []string{"function"}, PathPrefix: "src/"}

	if !f.Match("go", "function", "src/a.go") {
		t.Error("matching document was rejected")
	}
	if f.Match("python", "function", "src/a.go") {
		t.Error("wrong language accepted")
	}
	if f.Match("go", "struct", "src/a.go") {
		t.Error("wrong kind accepted")
	}
	if f.Match("go", "function", "lib/a.go") {
		t.Error("wrong path accepted")
	}
	// An unannotated document cannot be shown to satisfy the filter.
	if f.Match("", "function", "src/a.go") {
		t.Error("document without a language accepted by a language filter")
	}
	if (Filters{}).Match("", "", "") == false {
		t.Error("empty filters must accept everything")
	}
}
