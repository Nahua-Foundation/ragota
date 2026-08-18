// The alias subsystem: per-file tracking of `x := y` style bindings so an
// edge can carry the handful of aliases a trace needs to dereference it.
package ast

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"

	sitter "github.com/smacker/go-tree-sitter"
)

// constResolver maps identifiers to string literal values collected from the
// file and resolves expressions to strings: literal unquoting first, then a
// constant lookup.
type constResolver map[string]string

func (c constResolver) resolve(expr string) (string, bool) {
	if v, ok := unquote(expr); ok {
		return v, true
	}
	if v, ok := c[strings.TrimSpace(expr)]; ok {
		return v, true
	}
	return "", false
}

// Alias limits: per-file collection cap (giant generated files) and per-edge
// attachment cap (keep Edge.Meta small).
const (
	maxFileAliases = 64
	maxEdgeAliases = 8
)

// aliasLiterals are RHS texts that look identifier-like but are language
// literals/keywords, never aliases.
var aliasLiterals = map[string]bool{
	"true": true, "false": true, "nil": true, "null": true,
	"None": true, "True": true, "False": true, "undefined": true, "this": true,
}

// aliasScopeTypes are the grammar node types that delimit a local alias scope.
// The set is the union over the supported languages; type names are distinct
// enough between grammars that a shared table cannot mis-scope.
var aliasScopeTypes = map[string]bool{
	// go
	"function_declaration": true, "method_declaration": true, "func_literal": true,
	// python
	"function_definition": true, "lambda": true,
	// typescript / javascript
	"generator_function_declaration": true, "function_expression": true,
	"arrow_function": true, "function": true, "method_definition": true,
	// java / kotlin
	"constructor_declaration": true, "lambda_expression": true,
	"anonymous_function": true, "lambda_literal": true,
	// c#
	"local_function_statement": true,
}

// aliasEntry is one recorded alias plus the byte range it is visible in.
type aliasEntry struct {
	name  string
	expr  string
	start int
	end   int
}

// aliasTable collects local aliases (x := userID) together with the byte range
// of the declaration that encloses them. Aliases from different functions in
// the same file never mix, so `id := req.UserID` in one function cannot be
// attached to a call in another.
type aliasTable struct {
	entries []aliasEntry
}

// record adds name -> expr scoped to the innermost function-like ancestor of
// n, respecting the per-file cap. Self-aliases and literal keywords are
// dropped. An assignment outside any function is file-scoped.
func (t *aliasTable) record(n *sitter.Node, name, expr string) {
	name, expr = strings.TrimSpace(name), strings.TrimSpace(expr)
	if name == "" || expr == "" || name == expr || aliasLiterals[expr] {
		return
	}
	if len(t.entries) >= maxFileAliases {
		return
	}
	start, end := aliasScope(n)
	t.entries = append(t.entries, aliasEntry{name: name, expr: expr, start: start, end: end})
}

// at returns the aliases visible at byte offset pos. The innermost scope wins
// for a given name; within one scope the last recorded value wins.
func (t *aliasTable) at(pos int) map[string]string {
	if t == nil || len(t.entries) == 0 {
		return nil
	}
	out := map[string]string{}
	width := map[string]int{}
	for _, e := range t.entries {
		if pos < e.start || pos >= e.end {
			continue
		}
		w := e.end - e.start
		if prev, seen := width[e.name]; seen && prev < w {
			continue
		}
		out[e.name], width[e.name] = e.expr, w
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// relevant is at() narrowed to the aliases the call site actually references.
func (t *aliasTable) relevant(pos int, args []string, fields map[string]string) map[string]string {
	return relevantAliases(t.at(pos), args, fields)
}

// aliasScope returns the byte range of the innermost function-like ancestor of
// n, or the whole file when there is none.
func aliasScope(n *sitter.Node) (int, int) {
	for p := n; p != nil; p = p.Parent() {
		if aliasScopeTypes[p.Type()] {
			return int(p.StartByte()), int(p.EndByte())
		}
	}
	return 0, 1 << 62
}

// isAliasExpr reports whether a textual RHS looks like a plain identifier or
// dotted member access ("userId", "body.UserID"): identifier characters and
// dots only, not starting with a quote or digit. Used where the extractor has
// no AST node for the value (C#).
func isAliasExpr(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] >= '0' && s[0] <= '9') || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !contract.IsWordByte(s[i]) && s[i] != '.' {
			return false
		}
	}
	return true
}

// relevantAliases returns the subset of aliases whose NAME occurs as an
// identifier token in any of the argument expressions or field values,
// capped at maxEdgeAliases entries. Returns nil when nothing is relevant.
//
// One transitive step is included: for each directly relevant alias, aliases
// whose names occur in its VALUE are attached too, so a chain like
// `y := userID; x := y; g(x)` fits into a single edge's meta and the trace
// can dereference it end to end.
//
// Selection is deterministic under the cap: args are scanned in order, field
// values in sorted key order, and the transitive step in sorted alias-name
// order, so re-indexing the same file always stores the same subset.
func relevantAliases(aliases map[string]string, args []string, fields map[string]string) map[string]string {
	if len(aliases) == 0 {
		return nil
	}
	out := map[string]string{}
	var direct []string
	scan := func(expr string) {
		for _, tok := range contract.IdentTokens(expr) {
			if len(out) >= maxEdgeAliases {
				return
			}
			if v, ok := aliases[tok]; ok {
				if _, dup := out[tok]; !dup {
					direct = append(direct, tok)
				}
				out[tok] = v
			}
		}
	}
	for _, a := range args {
		scan(a)
	}
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		scan(fields[k])
	}
	// One transitive step over the values of the directly relevant aliases,
	// in sorted name order so the cap always keeps the same entries.
	sort.Strings(direct)
	for _, name := range direct {
		scan(out[name])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
