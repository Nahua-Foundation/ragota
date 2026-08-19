package store

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/repos"
)

func intOrZero(s string) any {
	if s == "" {
		return int64(0)
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return int64(0)
	}
	return i
}

func TestDialectPlaceholders(t *testing.T) {
	sq := SQLiteDialect{}
	for _, n := range []int{1, 2, 10} {
		if got := sq.Placeholder(n); got != "?" {
			t.Errorf("SQLiteDialect.Placeholder(%d) = %q, want %q", n, got, "?")
		}
	}
	pg := PostgresDialect{}
	for n, want := range map[int]string{1: "$1", 2: "$2", 10: "$10"} {
		if got := pg.Placeholder(n); got != want {
			t.Errorf("PostgresDialect.Placeholder(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{"100%", `100\%`},
		{"a_b", `a\_b`},
		{`back\slash`, `back\\slash`},
		{`%_\`, `\%\_\\`},
	}
	for _, tt := range tests {
		if got := EscapeLike(tt.in); got != tt.want {
			t.Errorf("EscapeLike(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuilderMethods(t *testing.T) {
	tests := []struct {
		name     string
		d        Dialect
		build    func(b *Builder) string // returns extra fragment (e.g. LIMIT)
		want     string
		wantFrag string
		wantArgs []any
	}{
		{
			name: "empty",
			d:    SQLiteDialect{},
			build: func(b *Builder) string {
				return ""
			},
			want:     "",
			wantArgs: nil,
		},
		{
			name: "eq sqlite",
			d:    SQLiteDialect{},
			build: func(b *Builder) string {
				b.Eq("repo_id", "r1")
				return ""
			},
			want:     " AND repo_id = ?",
			wantArgs: []any{"r1"},
		},
		{
			name: "eq postgres",
			d:    PostgresDialect{},
			build: func(b *Builder) string {
				b.Eq("repo_id", "r1")
				b.Eq("kind", "function")
				return ""
			},
			want:     " AND repo_id = $1 AND kind = $2",
			wantArgs: []any{"r1", "function"},
		},
		{
			name: "in sqlite",
			d:    SQLiteDialect{},
			build: func(b *Builder) string {
				b.In("kind", []any{"a", "b", "c"})
				return ""
			},
			want:     " AND kind IN (?,?,?)",
			wantArgs: []any{"a", "b", "c"},
		},
		{
			name: "in postgres numbered after eq",
			d:    PostgresDialect{},
			build: func(b *Builder) string {
				b.Eq("repo_id", "r1")
				b.In("kind", []any{"a", "b"})
				return ""
			},
			want:     " AND repo_id = $1 AND kind IN ($2,$3)",
			wantArgs: []any{"r1", "a", "b"},
		},
		{
			name: "in empty is noop",
			d:    SQLiteDialect{},
			build: func(b *Builder) string {
				b.In("kind", nil)
				return ""
			},
			want:     "",
			wantArgs: nil,
		},
		{
			// Suffix matching must not go through LIKE: its case sensitivity
			// and its wildcards differ between the backends.
			name: "suffix sqlite",
			d:    SQLiteDialect{},
			build: func(b *Builder) string {
				b.Suffix("qualified", "pkg_x.Foo")
				return ""
			},
			want:     " AND SUBSTR(qualified, -9) = ?",
			wantArgs: []any{"pkg_x.Foo"},
		},
		{
			name: "suffix postgres",
			d:    PostgresDialect{},
			build: func(b *Builder) string {
				b.Suffix("qualified", "100%")
				return ""
			},
			want:     " AND RIGHT(qualified, 4) = $1",
			wantArgs: []any{"100%"},
		},
		{
			name: "suffix empty is noop",
			d:    SQLiteDialect{},
			build: func(b *Builder) string {
				b.Suffix("qualified", "")
				return ""
			},
			want:     "",
			wantArgs: nil,
		},
		{
			name: "raw",
			d:    SQLiteDialect{},
			build: func(b *Builder) string {
				b.Raw("dst_id = 0")
				return ""
			},
			want:     " AND dst_id = 0",
			wantArgs: nil,
		},
		{
			// Both sides are folded by the engine rather than in Go, so the
			// comparison stays consistent with itself on either backend.
			name: "eq fold single column",
			d:    SQLiteDialect{},
			build: func(b *Builder) string {
				b.EqFold([]string{"name"}, "charge")
				return ""
			},
			want:     " AND LOWER(name) = LOWER(?)",
			wantArgs: []any{"charge"},
		},
		{
			// One term against two columns is one OR-ed condition, not two
			// AND-ed ones: it must widen the result, not narrow it.
			name: "eq fold any of two columns",
			d:    PostgresDialect{},
			build: func(b *Builder) string {
				b.Eq("repo_id", "r1")
				b.EqFold([]string{"name", "qualified"}, "ShipOrder")
				return ""
			},
			want:     " AND repo_id = $1 AND (LOWER(name) = LOWER($2) OR LOWER(qualified) = LOWER($3))",
			wantArgs: []any{"r1", "ShipOrder", "ShipOrder"},
		},
		{
			name: "contains fold escapes the term's wildcards",
			d:    SQLiteDialect{},
			build: func(b *Builder) string {
				b.ContainsFold([]string{"name"}, "get_user")
				return ""
			},
			want:     ` AND LOWER(name) LIKE LOWER(?) ESCAPE '\'`,
			wantArgs: []any{`%get\_user%`},
		},
		{
			name: "contains fold any of two columns",
			d:    PostgresDialect{},
			build: func(b *Builder) string {
				b.ContainsFold([]string{"name", "qualified"}, "Ship")
				return ""
			},
			want: ` AND (LOWER(name) LIKE LOWER($1) ESCAPE '\'` +
				` OR LOWER(qualified) LIKE LOWER($2) ESCAPE '\')`,
			wantArgs: []any{"%Ship%", "%Ship%"},
		},
		{
			name: "not in",
			d:    PostgresDialect{},
			build: func(b *Builder) string {
				b.NotIn("id", []any{int64(3), int64(7)})
				return ""
			},
			want:     " AND id NOT IN ($1,$2)",
			wantArgs: []any{int64(3), int64(7)},
		},
		{
			name: "not in empty is noop",
			d:    SQLiteDialect{},
			build: func(b *Builder) string {
				b.NotIn("id", nil)
				return ""
			},
			want:     "",
			wantArgs: nil,
		},
		{
			name: "limit sqlite",
			d:    SQLiteDialect{},
			build: func(b *Builder) string {
				b.Eq("repo_id", "r1")
				return b.Limit(5)
			},
			want:     " AND repo_id = ?",
			wantFrag: " LIMIT ?",
			wantArgs: []any{"r1", 5},
		},
		{
			name: "limit postgres numbered after filters",
			d:    PostgresDialect{},
			build: func(b *Builder) string {
				b.Eq("repo_id", "r1")
				b.Eq("kind", "call")
				return b.Limit(10)
			},
			want:     " AND repo_id = $1 AND kind = $2",
			wantFrag: " LIMIT $3",
			wantArgs: []any{"r1", "call", 10},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBuilder(tt.d)
			frag := tt.build(b)
			where, args := b.Where()
			if where != tt.want {
				t.Errorf("Where() = %q, want %q", where, tt.want)
			}
			if frag != tt.wantFrag {
				t.Errorf("Limit fragment = %q, want %q", frag, tt.wantFrag)
			}
			if !argsEqual(args, tt.wantArgs) {
				t.Errorf("args = %#v, want %#v", args, tt.wantArgs)
			}
		})
	}
}

func TestUnitFilters(t *testing.T) {
	tests := []struct {
		name     string
		d        Dialect
		tier     UnitTier
		opts     domain.QueryOpts
		want     string
		wantArgs []any
	}{
		{
			name:     "empty opts",
			d:        SQLiteDialect{},
			opts:     domain.QueryOpts{},
			want:     "",
			wantArgs: nil,
		},
		{
			name: "all filters sqlite",
			d:    SQLiteDialect{},
			opts: domain.QueryOpts{
				RepoID:          "r1",
				FilePath:        "a/b.go",
				Language:        "go",
				Kind:            "function",
				Kinds:           []string{"function", "method"},
				Name:            "Foo",
				Qualified:       "pkg.Foo",
				QualifiedSuffix: "x_y.Foo",
			},
			want: " AND repo_id = ? AND file_path = ? AND language = ? AND kind = ?" +
				" AND kind IN (?,?) AND LOWER(name) = LOWER(?) AND LOWER(qualified) = LOWER(?)" +
				" AND SUBSTR(qualified, -7) = ?",
			wantArgs: []any{"r1", "a/b.go", "go", "function", "function", "method", "Foo", "pkg.Foo", "x_y.Foo"},
		},
		{
			name: "all filters postgres",
			d:    PostgresDialect{},
			opts: domain.QueryOpts{
				RepoID:          "r1",
				FilePath:        "a/b.go",
				Language:        "go",
				Kind:            "function",
				Kinds:           []string{"function", "method"},
				Name:            "Foo",
				Qualified:       "pkg.Foo",
				QualifiedSuffix: "x_y.Foo",
			},
			want: " AND repo_id = $1 AND file_path = $2 AND language = $3 AND kind = $4" +
				" AND kind IN ($5,$6) AND LOWER(name) = LOWER($7) AND LOWER(qualified) = LOWER($8)" +
				" AND RIGHT(qualified, 7) = $9",
			wantArgs: []any{"r1", "a/b.go", "go", "function", "function", "method", "Foo", "pkg.Foo", "x_y.Foo"},
		},
		{
			// Name and Qualified narrow together; the one-term form widens.
			// Setting both alongside it is the caller's business — the point is
			// that the OR-ed condition is one condition.
			name:     "name or qualified is one condition",
			d:        SQLiteDialect{},
			opts:     domain.QueryOpts{NameOrQualified: "ShipOrder"},
			want:     " AND (LOWER(name) = LOWER(?) OR LOWER(qualified) = LOWER(?))",
			wantArgs: []any{"ShipOrder", "ShipOrder"},
		},
		{
			name: "substring tier loosens only the name terms",
			d:    PostgresDialect{},
			tier: TierSubstring,
			opts: domain.QueryOpts{
				RepoID:          "r1",
				Kind:            "function",
				NameOrQualified: "Ship",
				QualifiedSuffix: "x.Foo",
			},
			want: " AND repo_id = $1 AND kind = $2" +
				` AND (LOWER(name) LIKE LOWER($3) ESCAPE '\'` +
				` OR LOWER(qualified) LIKE LOWER($4) ESCAPE '\')` +
				" AND RIGHT(qualified, 5) = $5",
			wantArgs: []any{"r1", "function", "%Ship%", "%Ship%", "x.Foo"},
		},
		{
			name:     "subset keeps order",
			d:        PostgresDialect{},
			opts:     domain.QueryOpts{Language: "go", QualifiedSuffix: "Foo"},
			want:     " AND language = $1 AND RIGHT(qualified, 3) = $2",
			wantArgs: []any{"go", "Foo"},
		},
		{
			// A set of repositories is one IN condition, which is what scoping a
			// lookup to a working set needs and what repo_id = alone cannot say.
			name:     "repo set sqlite",
			d:        SQLiteDialect{},
			opts:     domain.QueryOpts{Repos: []string{"r1", "r2"}, Name: "Foo"},
			want:     " AND repo_id IN (?,?) AND LOWER(name) = LOWER(?)",
			wantArgs: []any{"r1", "r2", "Foo"},
		},
		{
			// The set and the single repository narrow together rather than one
			// replacing the other: a caller holding both is asking about one
			// repository within what it may read, and a repository outside the set
			// must match nothing.
			name:     "repo set narrows with repo id",
			d:        PostgresDialect{},
			opts:     domain.QueryOpts{RepoID: "r1", Repos: []string{"r1", "r2"}, Kind: "function"},
			want:     " AND repo_id = $1 AND repo_id IN ($2,$3) AND kind = $4",
			wantArgs: []any{"r1", "r1", "r2", "function"},
		},
		{
			// Empty means everywhere. The scope covering every repository is left
			// empty rather than spelled out, so the statement is the one that ran
			// before any working set existed.
			name:     "empty repo set is noop",
			d:        SQLiteDialect{},
			opts:     domain.QueryOpts{Repos: nil, Name: "Foo"},
			want:     " AND LOWER(name) = LOWER(?)",
			wantArgs: []any{"Foo"},
		},
		{
			// The set restricts every tier, including the one that widens a name
			// to a substring: loosening what a term matches must not loosen which
			// repositories answer.
			name:     "repo set survives the substring tier",
			d:        SQLiteDialect{},
			tier:     TierSubstring,
			opts:     domain.QueryOpts{Repos: []string{"r1"}, Name: "Foo"},
			want:     ` AND repo_id IN (?) AND LOWER(name) LIKE LOWER(?) ESCAPE '\'`,
			wantArgs: []any{"r1", "%Foo%"},
		},
		{
			// Line containment is evaluated in SQL so a LIMIT cannot hide
			// units that sit late in a large file.
			name:     "line containment sqlite",
			d:        SQLiteDialect{},
			opts:     domain.QueryOpts{FilePath: "a.go", Line: 900},
			want:     " AND file_path = ? AND start_line <= ? AND end_line >= ?",
			wantArgs: []any{"a.go", 900, 900},
		},
		{
			name:     "line containment postgres",
			d:        PostgresDialect{},
			opts:     domain.QueryOpts{Line: 12},
			want:     " AND start_line <= $1 AND end_line >= $2",
			wantArgs: []any{12, 12},
		},
		{
			name:     "zero line is noop",
			d:        SQLiteDialect{},
			opts:     domain.QueryOpts{Line: 0, Name: "Foo"},
			want:     " AND LOWER(name) = LOWER(?)",
			wantArgs: []any{"Foo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBuilder(tt.d)
			UnitFilters(b, tt.opts, tt.tier)
			where, args := b.Where()
			if where != tt.want {
				t.Errorf("Where() = %q, want %q", where, tt.want)
			}
			if !argsEqual(args, tt.wantArgs) {
				t.Errorf("args = %#v, want %#v", args, tt.wantArgs)
			}
		})
	}
}

// TestUnitOrder checks the ranking clause is built from every marker the shared
// path judgment holds. The clause itself is generated, so what is worth
// asserting is that nothing is dropped on the way into SQL and that the tail
// ends on where a unit is rather than on when it was inserted.
func TestUnitOrder(t *testing.T) {
	if !strings.HasSuffix(unitOrder, " THEN 1 ELSE 0 END, LENGTH(name), "+UnitTieBreak) {
		t.Errorf("unitOrder does not end in the documented tie-break: %q", unitOrder)
	}
	// The tie-break has to name the row's location before it names its id, or
	// the order is the indexing goroutines' commit order again. Asserting the
	// leading columns rather than the whole string keeps this test about the
	// property and not about the exact column list.
	if !strings.HasPrefix(UnitTieBreak, "repo_id, file_path, start_line") ||
		!strings.HasSuffix(UnitTieBreak, ", id") {
		t.Errorf("UnitTieBreak = %q, want a location key ending in id", UnitTieBreak)
	}
	for _, m := range append(repos.GeneratedPathMarkers(), repos.TestPathMarkers()...) {
		if !strings.Contains(unitOrder, quoteLiteral("%"+EscapeLike(m)+"%")) {
			t.Errorf("marker %q is not part of the unit ranking", m)
		}
	}
	// A marker containing a LIKE wildcard must reach SQL escaped, or "_test."
	// would match "xtest." as well.
	if !strings.Contains(unitOrder, `'%\_test.%'`) {
		t.Errorf("the _test. marker reached SQL unescaped: %q", unitOrder)
	}
}

func TestEdgeFilters(t *testing.T) {
	tests := []struct {
		name     string
		d        Dialect
		opts     domain.QueryOpts
		want     string
		wantArgs []any
	}{
		{
			name:     "empty opts",
			d:        PostgresDialect{},
			opts:     domain.QueryOpts{},
			want:     "",
			wantArgs: nil,
		},
		{
			name: "all filters sqlite",
			d:    SQLiteDialect{},
			opts: domain.QueryOpts{
				RepoID:     "r1",
				FilePath:   "a/b.go",
				Kind:       "call",
				Kinds:      []string{"call", "import"},
				Name:       "topic:orders",
				SrcID:      "7",
				DstID:      "9",
				Unresolved: true,
			},
			want: " AND repo_id = ? AND file_path = ? AND kind = ? AND kind IN (?,?)" +
				" AND dst_name = ? AND src_id = ? AND dst_id = ? AND dst_id = 0",
			wantArgs: []any{"r1", "a/b.go", "call", "call", "import", "topic:orders", int64(7), int64(9)},
		},
		{
			name: "all filters postgres",
			d:    PostgresDialect{},
			opts: domain.QueryOpts{
				RepoID:     "r1",
				FilePath:   "a/b.go",
				Kind:       "call",
				Kinds:      []string{"call", "import"},
				Name:       "topic:orders",
				SrcID:      "7",
				DstID:      "9",
				Unresolved: true,
			},
			want: " AND repo_id = $1 AND file_path = $2 AND kind = $3 AND kind IN ($4,$5)" +
				" AND dst_name = $6 AND src_id = $7 AND dst_id = $8 AND dst_id = 0",
			wantArgs: []any{"r1", "a/b.go", "call", "call", "import", "topic:orders", int64(7), int64(9)},
		},
		{
			name:     "id converter applied to non-numeric ids",
			d:        SQLiteDialect{},
			opts:     domain.QueryOpts{SrcID: "not-a-number"},
			want:     " AND src_id = ?",
			wantArgs: []any{int64(0)},
		},
		{
			name:     "unresolved only",
			d:        PostgresDialect{},
			opts:     domain.QueryOpts{Unresolved: true},
			want:     " AND dst_id = 0",
			wantArgs: nil,
		},
		{
			// Edges ignore the repository set, and that is the design rather than
			// an omission: the cross-repository graph is the point of the system,
			// so an edge is followed into a dormant repository on purpose. Only
			// unit queries — what retrieval reads — narrow to a working set.
			name:     "repo set does not scope edges",
			d:        PostgresDialect{},
			opts:     domain.QueryOpts{Repos: []string{"r1"}, Kind: "call"},
			want:     " AND kind = $1",
			wantArgs: []any{"call"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBuilder(tt.d)
			EdgeFilters(b, tt.opts, intOrZero)
			where, args := b.Where()
			if where != tt.want {
				t.Errorf("Where() = %q, want %q", where, tt.want)
			}
			if !argsEqual(args, tt.wantArgs) {
				t.Errorf("args = %#v, want %#v", args, tt.wantArgs)
			}
		})
	}
}

// TestFullQueryShape verifies the composed query text, including the argument
// order the placeholders depend on: filters, then the exclusion set, then the
// limit.
func TestFullQueryShape(t *testing.T) {
	opts := domain.QueryOpts{RepoID: "r1", Kinds: []string{"function", "method"}, QualifiedSuffix: "Foo", Limit: 3}

	got, args := UnitQuery(SQLiteDialect{}, opts, TierExact, opts.Limit, nil)
	want := "SELECT " + UnitColumns + " FROM ast_units WHERE 1=1" +
		" AND repo_id = ? AND kind IN (?,?)" +
		" AND SUBSTR(qualified, -3) = ?" +
		unitOrder + " LIMIT ?"
	if got != want {
		t.Errorf("sqlite unit query = %q, want %q", got, want)
	}
	if !argsEqual(args, []any{"r1", "function", "method", "Foo", 3}) {
		t.Errorf("sqlite unit args = %#v", args)
	}

	got, args = UnitQuery(PostgresDialect{}, opts, TierExact, opts.Limit, nil)
	want = "SELECT " + UnitColumns + " FROM ast_units WHERE 1=1" +
		" AND repo_id = $1 AND kind IN ($2,$3)" +
		" AND RIGHT(qualified, 3) = $4" +
		unitOrder + " LIMIT $5"
	if got != want {
		t.Errorf("postgres unit query = %q, want %q", got, want)
	}
	if !argsEqual(args, []any{"r1", "function", "method", "Foo", 3}) {
		t.Errorf("postgres unit args = %#v", args)
	}

	// The fallback tier's own shape: the term is loosened, the rows the exact
	// tier returned are excluded, and the limit is what is left of the page.
	got, args = UnitQuery(PostgresDialect{}, domain.QueryOpts{Name: "Ship", Limit: 5},
		TierSubstring, 3, []any{int64(1), int64(2)})
	want = "SELECT " + UnitColumns + " FROM ast_units WHERE 1=1" +
		` AND LOWER(name) LIKE LOWER($1) ESCAPE '\'` +
		" AND id NOT IN ($2,$3)" +
		unitOrder + " LIMIT $4"
	if got != want {
		t.Errorf("postgres fallback query = %q, want %q", got, want)
	}
	if !argsEqual(args, []any{"%Ship%", int64(1), int64(2), 3}) {
		t.Errorf("postgres fallback args = %#v", args)
	}

	// TierRemainder binds the term a second time, to say in the ranking what
	// matching "as written" means. Those arguments sit between the filters' and
	// the limit's, which is where the clause sits in the statement.
	got, args = UnitQuery(PostgresDialect{}, domain.QueryOpts{NameOrQualified: "Ship", Limit: 5},
		TierRemainder, 3, []any{int64(1)})
	want = "SELECT " + UnitColumns + " FROM ast_units WHERE 1=1" +
		` AND (LOWER(name) LIKE LOWER($1) ESCAPE '\'` +
		` OR LOWER(qualified) LIKE LOWER($2) ESCAPE '\')` +
		" AND id NOT IN ($3)" +
		" ORDER BY " + penaltyRank +
		", CASE WHEN (LOWER(name) = LOWER($4) OR LOWER(qualified) = LOWER($5)) THEN 0 ELSE 1 END" +
		", LENGTH(name), " + UnitTieBreak + " LIMIT $6"
	if got != want {
		t.Errorf("postgres remainder query = %q, want %q", got, want)
	}
	if !argsEqual(args, []any{"%Ship%", "%Ship%", int64(1), "Ship", "Ship", 3}) {
		t.Errorf("postgres remainder args = %#v", args)
	}
}

// TestHandWrittenStatement pins the one difference between the symbol lookup's
// first statement and the plain exact tier: the penalized paths are excluded
// rather than merely sorted last. Sorting is not enough, because a full page
// ends the lookup — a page ORDER BY had put generated rows at the *end* of is
// still full, and the substring statement would never run.
func TestHandWrittenStatement(t *testing.T) {
	opts := domain.QueryOpts{RepoID: "r1", Name: "Charge", Limit: 2}

	exact, exactArgs := UnitQuery(SQLiteDialect{}, opts, TierExact, opts.Limit, nil)
	hand, handArgs := UnitQuery(SQLiteDialect{}, opts, TierHandWritten, opts.Limit, nil)

	want := strings.Replace(exact, unitOrder, " AND NOT ("+penaltyPath+")"+unitOrder, 1)
	if hand != want {
		t.Errorf("hand-written statement = %q, want %q", hand, want)
	}
	// The markers are written into the SQL rather than bound, so excluding them
	// must not shift a single placeholder.
	if !argsEqual(handArgs, exactArgs) {
		t.Errorf("hand-written args = %#v, want the exact tier's %#v", handArgs, exactArgs)
	}
}

// TestQueryUnitsPlans pins which statements each caller gets, and it pins them
// as text rather than as a count: the hand-written pair must reach only the
// callers that opted into the fallback, because the linker, the graph and the
// promote pass resolve symbols through this same function, and for them a
// re-ranked or widened result is a wrong answer rather than an approximate one.
// What the statements return is asserted against real SQL in the storage
// conformance suite.
func TestQueryUnitsPlans(t *testing.T) {
	d := SQLiteDialect{}
	tests := []struct {
		name  string
		opts  domain.QueryOpts
		first int        // rows the first statement returns
		want  []UnitTier // the tier of each statement issued, in order
	}{
		{
			name:  "no fallback requested",
			opts:  domain.QueryOpts{Name: "Ship", Limit: 4},
			first: 1,
			want:  []UnitTier{TierExact},
		},
		{
			// A bulk read is not a lookup: without a limit the second statement
			// would be a substring scan of the whole table.
			name:  "no limit, no fallback",
			opts:  domain.QueryOpts{Name: "Ship", Fallback: true},
			first: 0,
			want:  []UnitTier{TierExact},
		},
		{
			// Nothing names a symbol, so the second statement would repeat the
			// first one's rows.
			name:  "no term to loosen",
			opts:  domain.QueryOpts{RepoID: "r1", Limit: 4, Fallback: true},
			first: 0,
			want:  []UnitTier{TierExact},
		},
		{
			// A page's worth of hand-written code matched as written, so nothing
			// the substring scan could find belongs on it.
			name:  "hand-written statement filled the page",
			opts:  domain.QueryOpts{NameOrQualified: "Ship", Limit: 2, Fallback: true},
			first: 2,
			want:  []UnitTier{TierHandWritten},
		},
		{
			name:  "short page tops up",
			opts:  domain.QueryOpts{NameOrQualified: "Ship", Limit: 4, Fallback: true},
			first: 2,
			want:  []UnitTier{TierHandWritten, TierRemainder},
		},
	}
	rows := func(ids ...string) []*domain.ASTUnit {
		out := make([]*domain.ASTUnit, 0, len(ids))
		for _, id := range ids {
			out = append(out, &domain.ASTUnit{ID: id, Name: "Ship"})
		}
		return out
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var queries []string
			got, err := QueryUnits(d, tt.opts, intOrZero,
				func(query string, _ []any) ([]*domain.ASTUnit, error) {
					queries = append(queries, query)
					if len(queries) == 1 {
						return rows(firstIDs(tt.first)...), nil
					}
					return rows("second"), nil
				})
			if err != nil {
				t.Fatalf("QueryUnits: %v", err)
			}
			if len(queries) != len(tt.want) {
				t.Fatalf("issued %d statements (%v), want %d", len(queries), queries, len(tt.want))
			}
			for i, tier := range tt.want {
				limit, exclude := tt.opts.Limit, []any(nil)
				if i > 0 {
					limit = tt.opts.Limit - tt.first
					for _, id := range firstIDs(tt.first) {
						exclude = append(exclude, intOrZero(id))
					}
				}
				want, _ := UnitQuery(d, tt.opts, tier, limit, exclude)
				if queries[i] != want {
					t.Errorf("statement %d = %q, want %q", i, queries[i], want)
				}
			}
			// The second statement's rows follow the first one's rather than
			// being merged into them: the ranking that decides between the two
			// is the tier boundary itself.
			if len(tt.want) == 2 && (len(got) != tt.first+1 || got[len(got)-1].ID != "second") {
				t.Errorf("rows = %d, last id %q; want the second statement's row appended last",
					len(got), got[len(got)-1].ID)
			}
		})
	}
}

// firstIDs names the rows the first statement returns, so the expected
// exclusion set can be rebuilt from the same source.
func firstIDs(n int) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, strconv.Itoa(i))
	}
	return out
}

func argsEqual(got, want []any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i]) {
			return false
		}
	}
	return true
}
