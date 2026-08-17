package contract

import "strings"

// Identifier and signature vocabulary: the normalizations two sides of a join
// have to agree on. The indexer writes identifiers and signatures at parse
// time and the graph reads them back when tracing a value across a call, so
// both go through the functions here rather than each spelling out its own
// rule — the same reason ReducePath lives in this package.

// NormIdent normalizes an identifier for cross-language matching:
// lowercased with underscores removed, so UserId == user_id == userId.
func NormIdent(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "_", ""))
}

// ParamNames extracts parameter names from a stored signature for a language.
// Signatures are the raw parameter-list text captured at parse time.
func ParamNames(language, signature string) []string {
	sig := strings.TrimSpace(signature)
	sig = strings.TrimPrefix(sig, "(")
	sig = strings.TrimSuffix(sig, ")")
	if sig == "" {
		return nil
	}
	parts := SplitTopLevel(sig, ',')
	var names []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Default value: the name is left of a top-level '='
		// ("int x = 5", "amount: number = 0").
		if pieces := SplitTopLevel(part, '='); len(pieces) > 1 {
			part = strings.TrimSpace(pieces[0])
			if part == "" {
				continue
			}
		}
		switch language {
		case "java", "csharp":
			// "Type name", "final Type name", "[FromBody] Type name",
			// varargs "String... names" / "String ...names"
			fields := strings.Fields(part)
			if len(fields) > 0 {
				name := fields[len(fields)-1]
				names = append(names, strings.TrimLeft(name, "*&@."))
			}
		default:
			// Go / TS: "name Type", "name: Type", "name",
			// variadic "args ...string", rest "...args: string[]"
			name := part
			if i := strings.IndexAny(part, " :?"); i > 0 {
				name = part[:i] // "uri?: string" — the optional marker is not part of the name
			}
			names = append(names, strings.TrimLeft(name, "*&."))
		}
	}
	// Python bound methods carry the receiver in the parameter list but not in
	// the argument list: the tracer maps argument index 0 onto the first real
	// parameter, so `def process(self, user_id)` must yield ["user_id"].
	if language == "python" && len(names) > 0 && (names[0] == "self" || names[0] == "cls") {
		names = names[1:]
	}
	return names
}

// SplitTopLevel splits s on sep, ignoring separators nested inside brackets or
// a generic parameter list. Signature parsing and the extractors' expression
// splitting share it so that "Map<String, Int> m" counts as one parameter in
// both.
func SplitTopLevel(s string, sep byte) []string {
	var parts []string
	depth := 0
	angleDepth := 0
	last := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '<':
			if i > 0 && isWordByte(s[i-1]) && i+1 < len(s) && s[i+1] != '=' && s[i+1] != '<' {
				angleDepth++
			}
		case '>':
			if angleDepth > 0 {
				angleDepth--
			}
		case sep:
			if depth == 0 && angleDepth == 0 {
				parts = append(parts, s[last:i])
				last = i + 1
			}
		}
	}
	parts = append(parts, s[last:])
	return parts
}

// isWordByte reports whether b is an identifier character.
func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// IdentTokens splits s on anything that cannot appear in an identifier.
func IdentTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
}

// TokenComponents splits s into identifier tokens and each token into its
// lower-cased word components, keeping the tokens apart.
func TokenComponents(s string) [][]string {
	toks := IdentTokens(s)
	out := make([][]string, 0, len(toks))
	for _, tok := range toks {
		if comps := splitComponents(tok); len(comps) > 0 {
			out = append(out, comps)
		}
	}
	return out
}

// WordComponents is TokenComponents flattened: every word component of s, in
// order, lower-cased. "OrderItemRepo" and "order_item_repo" both yield
// [order item repo], which is what lets one side of a join match the other
// however each spelled it.
func WordComponents(s string) []string {
	var out []string
	for _, comps := range TokenComponents(s) {
		out = append(out, comps...)
	}
	return out
}

func splitComponents(tok string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = nil
		}
	}
	rs := []rune(tok)
	for i, r := range rs {
		if r == '_' {
			flush()
			continue
		}
		// An upper-case letter opens a new component when it follows a
		// lower-case one ("userId") or closes an acronym ("HTTPServer").
		if isUpper(r) && i > 0 && (!isUpper(rs[i-1]) || (i+1 < len(rs) && isLower(rs[i+1]))) {
			flush()
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isLower(r rune) bool { return r >= 'a' && r <= 'z' }
