package graph

import "github.com/Nahua-Foundation/ragota/internal/contract"

// Identifier segmentation shared by config-key matching (linker) and
// parameter-flow matching (trace). Matching is component-based rather than
// substring-based: "user" must not match "username" or "user_agent".
//
// The segmentation itself lives in internal/contract, with the other
// normalizations both sides of a join have to agree on. These are the graph's
// local spellings of it.

// identTokens splits a string into identifier tokens: maximal runs of letters,
// digits and '_' ("g(x, a.b)" -> ["g", "x", "a", "b"]).
func identTokens(s string) []string { return contract.IdentTokens(s) }

// tokenComponents splits every identifier token of s into its lowercase word
// components: "req.GetUserID()" -> [["req"], ["get", "user", "id"]]. Token
// grouping is preserved because a match must end at a token boundary.
func tokenComponents(s string) [][]string { return contract.TokenComponents(s) }

// WordComponents is the exported form of wordComponents, for callers outside
// the graph package (the service layer matches unit names against query words
// with the same rules the linker uses, so "user" never matches "username").
func WordComponents(s string) []string { return contract.WordComponents(s) }

// wordComponents is tokenComponents flattened, dropping token boundaries:
// "kafka.orders-topic" and "ORDERS_TOPIC" both end with ["orders", "topic"].
func wordComponents(s string) []string { return contract.WordComponents(s) }

// hasComponentSuffix reports whether seq ends with the components of suffix.
func hasComponentSuffix(seq, suffix []string) bool {
	if len(suffix) == 0 || len(suffix) > len(seq) {
		return false
	}
	off := len(seq) - len(suffix)
	for i, c := range suffix {
		if seq[off+i] != c {
			return false
		}
	}
	return true
}
