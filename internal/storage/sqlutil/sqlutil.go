// Package sqlutil provides shared SQL query-building helpers for the SQL
// storage backends (sqlite, postgres). It centralizes the WHERE-filter logic
// for AST unit and edge queries so the backends cannot silently drift apart;
// only placeholder syntax differs between them, expressed via Dialect.
package sqlutil

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// Column lists shared verbatim by both SQL backends.
const (
	UnitColumns = "id, repo_id, file_path, language, kind, name, qualified, parent_id, start_line, end_line, start_byte, end_byte, signature, doc, hash, meta"
	EdgeColumns = "id, repo_id, src_id, dst_id, kind, dst_name, file_path, line, dst_repo_id, confidence, meta"
)

// IntOrZero converts a string unit ID to its integer form; empty or
// unparsable -> 0 (unresolved). Both SQL backends store unit IDs as integers
// and hand this to the query builders as their idConv.
func IntOrZero(s string) int64 {
	if s == "" {
		return 0
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return i
}

// EdgeOrder is what both backends order GetEdges by. Like UnitTieBreak, it says
// an edge's place in the result is where the edge is, not when it was written:
// `ORDER BY id` handed back the order the indexing goroutines happened to
// commit rows in, and every caller that takes a prefix of the result then saw a
// different *set* between two passes over identical sources, not merely a
// different order within one. That is not theoretical: promote's callers path
// reads callerEdgeLimit (50) edges per destination name, and fourteen names in
// the boutique/petclinic/robotshop corpus alone have more than that.
//
// The leading term is file_path rather than repo_id because the plan has to
// survive: with repo_id first SQLite abandons idx_edges_repo_kind for
// idx_edges_repo_file and walks every edge of the repository instead of the
// kinds asked for. Ordering as written keeps the index the filter chose and
// costs the same temp b-tree `ORDER BY id` already paid for. repo_id is still
// in the key, late, since two repositories can hold the same path.
//
// id remains last for totality within one database. Reaching it means two rows
// agree on repository, path, line, kind, destination and metadata — rows no
// caller can tell apart.
const EdgeOrder = " ORDER BY file_path, line, kind, dst_name, meta, repo_id, id"

// Dialect abstracts syntax differences between SQL engines.
type Dialect interface {
	// Placeholder returns the placeholder for the n-th argument (1-based).
	Placeholder(n int) string
	// SuffixCond returns a condition matching rows whose col ends with the
	// runes bound at ph, of which there are n.
	SuffixCond(col, ph string, n int) string
}

// SQLiteDialect uses "?" placeholders.
type SQLiteDialect struct{}

// Placeholder implements Dialect.
func (SQLiteDialect) Placeholder(int) string { return "?" }

// SuffixCond implements Dialect. SQLite's LIKE is case-insensitive for ASCII
// while Postgres' is case-sensitive, so suffix matching goes through SUBSTR
// instead: qualified names are identifiers and must compare case-sensitively
// on every backend.
func (SQLiteDialect) SuffixCond(col, ph string, n int) string {
	return "SUBSTR(" + col + ", -" + strconv.Itoa(n) + ") = " + ph
}

// PostgresDialect uses positional "$n" placeholders.
type PostgresDialect struct{}

// Placeholder implements Dialect.
func (PostgresDialect) Placeholder(n int) string { return "$" + strconv.Itoa(n) }

// SuffixCond implements Dialect; see SQLiteDialect.SuffixCond.
func (PostgresDialect) SuffixCond(col, ph string, n int) string {
	return "RIGHT(" + col + ", " + strconv.Itoa(n) + ") = " + ph
}

// Builder accumulates WHERE conditions and their arguments for one query.
type Builder struct {
	d     Dialect
	conds []string
	args  []any
}

// NewBuilder returns a Builder for the given dialect.
func NewBuilder(d Dialect) *Builder { return &Builder{d: d} }

// next registers v as the next argument and returns its placeholder.
func (b *Builder) next(v any) string {
	b.args = append(b.args, v)
	return b.d.Placeholder(len(b.args))
}

// Eq adds a "col = <placeholder>" condition.
func (b *Builder) Eq(col string, v any) {
	b.conds = append(b.conds, col+" = "+b.next(v))
}

// EqFold adds a case-insensitive equality condition satisfied when any of cols
// equals v. Several columns become one OR-ed condition rather than several
// AND-ed ones, which is what lets a caller hold a single term and ask whether
// it is either of two names.
//
// Both sides go through SQL's LOWER rather than being folded in Go: the engines
// disagree with each other and with Go about non-ASCII case, and only lowering
// column and argument with the same function keeps the comparison consistent
// with itself. For the same reason this does not use LIKE, whose case
// sensitivity differs between the backends (see SQLiteDialect.SuffixCond).
func (b *Builder) EqFold(cols []string, v string) {
	b.anyOf(cols, func(col string) string {
		return "LOWER(" + col + ") = LOWER(" + b.next(v) + ")"
	})
}

// ContainsFold adds a case-insensitive substring condition satisfied when any
// of cols contains v. The term's own LIKE wildcards are escaped, so a symbol
// named "100%" is matched literally.
func (b *Builder) ContainsFold(cols []string, v string) {
	pattern := "%" + EscapeLike(v) + "%"
	b.anyOf(cols, func(col string) string {
		return "LOWER(" + col + ") LIKE LOWER(" + b.next(pattern) + `) ESCAPE '\'`
	})
}

// anyOf adds the OR of cond over cols as a single condition, parenthesized when
// there is more than one so it cannot bind loosely against the AND-ed filters
// around it.
func (b *Builder) anyOf(cols []string, cond func(col string) string) {
	if len(cols) == 0 {
		return
	}
	parts := make([]string, 0, len(cols))
	for _, col := range cols {
		parts = append(parts, cond(col))
	}
	if len(parts) == 1 {
		b.conds = append(b.conds, parts[0])
		return
	}
	b.conds = append(b.conds, "("+strings.Join(parts, " OR ")+")")
}

// NotIn adds a "col NOT IN (<placeholders>)" condition. It is a no-op for
// empty vals.
func (b *Builder) NotIn(col string, vals []any) {
	if len(vals) == 0 {
		return
	}
	ph := make([]string, 0, len(vals))
	for _, v := range vals {
		ph = append(ph, b.next(v))
	}
	b.conds = append(b.conds, col+" NOT IN ("+strings.Join(ph, ",")+")")
}

// In adds a "col IN (<placeholders>)" condition. It is a no-op for empty vals.
func (b *Builder) In(col string, vals []any) {
	if len(vals) == 0 {
		return
	}
	ph := make([]string, 0, len(vals))
	for _, v := range vals {
		ph = append(ph, b.next(v))
	}
	b.conds = append(b.conds, col+" IN ("+strings.Join(ph, ",")+")")
}

// Suffix adds a condition matching values of col that end with suffix. The
// comparison is exact and case-sensitive on every backend; suffix is bound as
// a plain argument, so it needs no wildcard escaping.
func (b *Builder) Suffix(col, suffix string) {
	if suffix == "" {
		return
	}
	b.conds = append(b.conds, b.d.SuffixCond(col, b.next(suffix), utf8.RuneCountInString(suffix)))
}

// Lte adds a "col <= <placeholder>" condition.
func (b *Builder) Lte(col string, v any) {
	b.conds = append(b.conds, col+" <= "+b.next(v))
}

// Gte adds a "col >= <placeholder>" condition.
func (b *Builder) Gte(col string, v any) {
	b.conds = append(b.conds, col+" >= "+b.next(v))
}

// Raw adds a condition verbatim, without arguments.
func (b *Builder) Raw(cond string) {
	b.conds = append(b.conds, cond)
}

// Limit registers n as the next argument and returns a " LIMIT <placeholder>"
// fragment to append after ORDER BY. Call it after all filter methods so the
// argument order matches the query text; the argument itself is returned by
// Where along with the filter arguments.
func (b *Builder) Limit(n int) string {
	return " LIMIT " + b.next(n)
}

// Where returns the accumulated conditions as an " AND c1 AND c2..." fragment
// (empty when no conditions were added), suitable for appending to a query
// ending in "WHERE 1=1" or "WHERE TRUE", plus all registered arguments in
// order.
func (b *Builder) Where() (string, []any) {
	if len(b.conds) == 0 {
		return "", b.args
	}
	return " AND " + strings.Join(b.conds, " AND "), b.args
}

// UnitTier selects how one statement of a unit query matches the name terms in
// its options, which paths it is willing to return, and how it ranks what it
// finds. A lookup runs two of them in sequence; which two is QueryUnits'
// decision.
type UnitTier int

const (
	// TierExact matches names as written, up to case.
	TierExact UnitTier = iota
	// TierSubstring matches names that contain the term.
	TierSubstring
	// TierHandWritten matches as TierExact does but returns only the paths the
	// ranking does not penalize; TierRemainder, which follows it, matches as
	// TierSubstring does and ranks a generated or test path below every
	// hand-written row — including below one the term merely occurs in.
	//
	// The pair exists because in TierExact/TierSubstring the path penalty is
	// subordinate to exactness: it orders rows within a tier and cannot lift one
	// across the boundary. "charge" is an exact match in a dozen generated gRPC
	// stubs, so those dozen fill the page in TierExact, TierSubstring never runs
	// because the page is not short, and a hand-written charge the term only
	// occurs in is never reached at all. For an agent that named a symbol from
	// the wording of a question, a hand-written near miss is a better answer
	// than a generated exact one. For the linker resolving an edge it is not,
	// which is why this is a second pair rather than a change to the first.
	TierHandWritten
	TierRemainder
)

// substring reports whether the tier matches a name term by containment rather
// than as written. The symbol lookup's two statements match the same two ways
// the plain tiers do; what makes them a different plan is which paths they
// return and how they rank them.
func (t UnitTier) substring() bool { return t == TierSubstring || t == TierRemainder }

// UnitFilters applies the shared AST-unit query filters from opts to b. The
// name terms (Name, Qualified, NameOrQualified) are matched according to tier;
// every other filter is a hard restriction and is applied the same way in all
// of them.
//
// Name and qualified matching ignores case. An agent writes a symbol the way
// the question phrases it — "which code charges the card" gives "charge" — and
// an index holding "Charge" answered that with nothing at all.
func UnitFilters(b *Builder, opts storage.QueryOpts, tier UnitTier) {
	if opts.RepoID != "" {
		b.Eq("repo_id", opts.RepoID)
	}
	// The set is a second, independent restriction rather than an alternative
	// spelling of RepoID: a caller may hold both, and then it is asking about one
	// repository *within* what it may read. Both statements of a lookup are
	// composed through here, so the tier that widens a name to a substring cannot
	// widen the repositories with it.
	if len(opts.Repos) > 0 {
		b.In("repo_id", toAnySlice(opts.Repos))
	}
	if opts.FilePath != "" {
		b.Eq("file_path", opts.FilePath)
	}
	if opts.Language != "" {
		b.Eq("language", opts.Language)
	}
	if opts.Kind != "" {
		b.Eq("kind", opts.Kind)
	}
	if len(opts.Kinds) > 0 {
		b.In("kind", toAnySlice(opts.Kinds))
	}
	unitTerms(b, opts, tier)
	// QualifiedSuffix stays exact and case-sensitive in every tier: it joins
	// cross-repo contract keys, where the two sides are built by the linker
	// rather than typed by anyone, and a looser match there resolves an edge to
	// the wrong symbol instead of merely ranking one badly.
	if opts.QualifiedSuffix != "" {
		b.Suffix("qualified", opts.QualifiedSuffix)
	}
	if opts.Line > 0 {
		b.Lte("start_line", opts.Line)
		b.Gte("end_line", opts.Line)
	}
	// A full TierHandWritten page ends the lookup, so it must not be filled by
	// the very rows the ranking exists to demote: a page ORDER BY had merely put
	// the generated rows at the *end* of is still a full page. The penalty is
	// therefore a filter here as well as a sort key. It does no sorting once it
	// is a filter — every surviving row scores 0 — but keeping the shared
	// ranking leaves every statement one shape, and the CASE only runs over the
	// rows the name index already narrowed to.
	if tier == TierHandWritten {
		b.Raw("NOT (" + penaltyPath + ")")
	}
}

// unitTerms applies opts' name terms at the given tier. They are their own
// function because TierRemainder has to build them a second time, at TierExact,
// to rank by them; see unitTermConds.
func unitTerms(b *Builder, opts storage.QueryOpts, tier UnitTier) {
	if opts.Name != "" {
		unitTerm(b, tier, []string{"name"}, opts.Name)
	}
	if opts.Qualified != "" {
		unitTerm(b, tier, []string{"qualified"}, opts.Qualified)
	}
	if opts.NameOrQualified != "" {
		unitTerm(b, tier, []string{"name", "qualified"}, opts.NameOrQualified)
	}
}

func unitTerm(b *Builder, tier UnitTier, cols []string, v string) {
	if tier.substring() {
		b.ContainsFold(cols, v)
		return
	}
	b.EqFold(cols, v)
}

// unitTermConds returns the conditions tier would apply to opts' name terms,
// registering their arguments in b but leaving b's WHERE clause untouched.
//
// TierRemainder ranks by whether a row would *also* have matched as written, so
// the exact conditions have to exist a second time, in the ORDER BY, while the
// WHERE holds the loosened ones. Building both from unitTerms is what keeps the
// two spellings of "matched exactly" from drifting: a hand-written copy would
// have to track EqFold's folding rules and the column pair each option matches
// against.
func unitTermConds(b *Builder, opts storage.QueryOpts, tier UnitTier) []string {
	start := len(b.conds)
	unitTerms(b, opts, tier)
	conds := slices.Clone(b.conds[start:])
	b.conds = b.conds[:start]
	return conds
}

// penaltyPath holds for a file path that is generated output, the interface
// definition it was generated from, or test, mock and vendored code — the parts
// of a repository a question is never about. It is a constant: the markers it is
// built from are this package's own, so they are written into the SQL rather
// than bound.
var penaltyPath = buildPenaltyPath()

// penaltyRank is penaltyPath as a sort key — 1 for the penalized paths, 0 for
// the rest — so that ORDER BY puts them last.
var penaltyRank = "CASE WHEN " + penaltyPath + " THEN 1 ELSE 0 END"

// unitOrder ranks AST units, and is what every statement but TierRemainder
// orders by, so that they all share one statement shape.
//
// Ordering by id ranked units by the order their files happened to be parsed.
// Asking the boutique corpus for ShipOrder returned ten generated gRPC stubs
// before src/shippingservice/main.go, the only hand-written implementation and
// the seventeenth of seventeen rows. Generated and test paths share one penalty
// because they are demoted for one reason — neither is the code a question is
// about — and splitting them would be a claim about their relative worth that
// nothing measured supports. Name length then puts the most precise candidate
// first, which is what separates the substring matches from each other, and
// UnitTieBreak settles what is left.
var unitOrder = " ORDER BY " + penaltyRank + ", LENGTH(name), " + UnitTieBreak

// UnitTieBreak settles rows that every ranking term above it scores equally.
//
// It has to be a function of the corpus, and `id` — the row's autoincrement
// key — is not one. Indexing runs on indexes.workers goroutines, so a row's id
// records which worker reached its file first; two passes over identical
// sources produce identical rows with different ids. That was not a
// hypothetical: `which entity maps to the visits table` promotes the three
// declarations of `db:visits` in petclinic, and all three tie here — same
// penalty (0), same LENGTH(name) (6, "visits") — so id alone ordered them.
// Visit.java, the answer, came back id 5959 against the schema files' 6050 and
// 6055 on one run and 7018 against 5571 and 5576 on the next, and the same
// question scored rank 1 or rank 3 accordingly. Where a unit *is* decides it
// instead: repository, file, and position within the file, which is what the
// corpus says and nothing else.
//
// id stays as the last term so the order is total inside one database. Reaching
// it means two rows agree on repository, path, position, kind, name and
// qualified name, and rows that agree on all of those are indistinguishable to
// every caller — whichever comes first, the hit is the same.
const UnitTieBreak = "repo_id, file_path, start_line, start_byte, kind, name, qualified, id"

func buildPenaltyPath() string {
	markers := append(repos.GeneratedPathMarkers(), repos.TestPathMarkers()...)
	conds := make([]string, 0, len(markers))
	for _, m := range markers {
		// The markers are already lower-case, so only the column is folded.
		conds = append(conds, `LOWER('/' || file_path) LIKE `+quoteLiteral("%"+EscapeLike(m)+"%")+` ESCAPE '\'`)
	}
	return strings.Join(conds, " OR ")
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// unitOrderBy returns the ORDER BY clause for one statement and registers in b
// whatever arguments it binds. Call it after every filter and before Limit: the
// clause sits between the WHERE and the LIMIT in the statement text, so its
// arguments have to sit between theirs.
//
// TierRemainder is the only statement that needs more than unitOrder, because it
// is the only one holding rows of several kinds at once: hand-written rows the
// term merely occurs in, generated rows that match it as written, and generated
// rows it merely occurs in. Exactness still ranks — within either path group a
// row matching as written comes first — but it ranks *below* the path penalty,
// which is the whole point of the plan. LENGTH(name) is no substitute for it:
// a term can match a row through its qualified name while the ranking reads its
// name, and then the shorter name is not the exact one.
func unitOrderBy(b *Builder, opts storage.QueryOpts, tier UnitTier) string {
	if tier != TierRemainder {
		return unitOrder
	}
	exact := unitTermConds(b, opts, TierExact)
	if len(exact) == 0 {
		// Nothing named a symbol, so no row matched "as written" in any sense.
		// QueryUnits never composes this statement without a term, but UnitQuery
		// is callable on its own and must not emit an empty CASE.
		return unitOrder
	}
	return " ORDER BY " + penaltyRank +
		", CASE WHEN " + strings.Join(exact, " AND ") + " THEN 0 ELSE 1 END" +
		", LENGTH(name), " + UnitTieBreak
}

// UnitQuery composes the complete AST-unit query for one tier: the shared
// filters, the tier's name matching, its ranking and the limit. exclude holds
// backend-form unit ids the query must skip, which is how a lookup's second
// statement leaves out what its first one already returned.
func UnitQuery(d Dialect, opts storage.QueryOpts, tier UnitTier, limit int, exclude []any) (string, []any) {
	b := NewBuilder(d)
	UnitFilters(b, opts, tier)
	b.NotIn("id", exclude)
	order := unitOrderBy(b, opts, tier)
	var lim string
	if limit > 0 {
		lim = b.Limit(limit)
	}
	where, args := b.Where()
	return "SELECT " + UnitColumns + " FROM ast_units WHERE 1=1" + where + order + lim, args
}

// UnitScanner runs one composed unit query and returns the rows it selects.
type UnitScanner func(query string, args []any) ([]*storage.ASTUnit, error)

// QueryUnits is the AST-unit lookup both SQL backends expose. It lives here
// rather than in either of them so that a change to the tiering or the ranking
// cannot land in one backend only.
//
// It runs at most two statements, and the second one only when the first came
// back short. That structure is what keeps a lookup cheap on a large index: the
// first statement compares a name for equality and is served by the lower-case
// name and qualified indexes, while the second one asks for LIKE '%term%', which
// no index can answer and which therefore costs a pass over the whole unit
// table. A caller whose first statement fills its page never pays for that pass.
//
// Which two statements run depends on the caller. Without Fallback the lookup is
// the exact tier alone: a caller resolving an edge wants the name it asked for
// or nothing. With Fallback it is the hand-written pair, and only a full page of
// hand-written code matching as written can end it early — anything short of
// that is worth the scan, because the rows that would otherwise have filled the
// page are generated stubs, and those are not what the question was about.
func QueryUnits(d Dialect, opts storage.QueryOpts, idConv func(string) any, scan UnitScanner) ([]*storage.ASTUnit, error) {
	first, second := TierExact, TierSubstring
	widen := opts.Fallback && opts.Limit > 0 && hasUnitTerm(opts)
	if widen {
		first, second = TierHandWritten, TierRemainder
	}
	query, args := UnitQuery(d, opts, first, opts.Limit, nil)
	units, err := scan(query, args)
	if err != nil {
		return nil, err
	}
	if !widen || len(units) >= opts.Limit {
		return units, nil
	}

	// A short first page is the complete set of hand-written rows matching as
	// written, so the second statement can rank everything else against itself
	// alone and fill what is left of the page.
	exclude := make([]any, 0, len(units))
	for _, u := range units {
		exclude = append(exclude, idConv(u.ID))
	}
	query, args = UnitQuery(d, opts, second, opts.Limit-len(units), exclude)
	more, err := scan(query, args)
	if err != nil {
		return nil, err
	}
	return append(units, more...), nil
}

// hasUnitTerm reports whether opts names a symbol at all. Without one there is
// nothing to loosen, so a second statement would return exactly the rows the
// first one already did.
func hasUnitTerm(opts storage.QueryOpts) bool {
	return opts.Name != "" || opts.Qualified != "" || opts.NameOrQualified != ""
}

// EdgeFilters applies the shared edge query filters from opts to b. idConv
// converts string unit IDs (SrcID/DstID) to the backend's stored form (both
// SQL backends store integer IDs; they pass their intOrZero converter).
func EdgeFilters(b *Builder, opts storage.QueryOpts, idConv func(string) any) {
	if opts.RepoID != "" {
		b.Eq("repo_id", opts.RepoID)
	}
	if opts.FilePath != "" {
		b.Eq("file_path", opts.FilePath)
	}
	if opts.Kind != "" {
		b.Eq("kind", opts.Kind)
	}
	if len(opts.Kinds) > 0 {
		b.In("kind", toAnySlice(opts.Kinds))
	}
	if opts.Name != "" {
		b.Eq("dst_name", opts.Name)
	}
	if opts.SrcID != "" {
		b.Eq("src_id", idConv(opts.SrcID))
	}
	if opts.DstID != "" {
		b.Eq("dst_id", idConv(opts.DstID))
	}
	if opts.Unresolved {
		b.Raw("dst_id = 0")
	}
}

// EscapeLike escapes SQL LIKE wildcards in s (used with ESCAPE '\').
func EscapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func toAnySlice(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}
