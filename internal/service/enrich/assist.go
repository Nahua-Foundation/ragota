package enrich

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/Nahua-Foundation/ragota/internal/llm"
)

// queryRewriteTimeout bounds a single query-rewrite LLM call.
const queryRewriteTimeout = 10 * time.Second

// maxRewrittenQueryLen caps the rewritten query. A model that ignores the
// instruction and answers in prose must not turn into a giant search query.
const maxRewrittenQueryLen = 200

// queryRewritePrompt asks the assistant LLM to turn a natural-language
// question into a keyword-style search query.
const queryRewritePrompt = "Rewrite this natural-language question about a codebase " +
	"into a short keyword-style search query (identifiers, code terms). " +
	"Reply with the query only."

// rewritePrefixes are the labels models like to put in front of their answer.
var rewritePrefixes = []string{
	"query:", "search query:", "rewritten query:", "rewrite:", "keywords:",
	"search:", "answer:", "output:", "result:",
}

// SetAssistant configures the auxiliary assistant LLM. queryRewrite enables
// rewriting natural-language queries into keyword-style search queries in
// BuildContext.
func (e *Enricher) SetAssistant(gen llm.Generator, queryRewrite bool) {
	e.assistGen = gen
	e.assistRewrite = queryRewrite
}

// RewriteQuery rewrites a natural-language query into a keyword-style search
// query using the assistant LLM. It reports whether a rewrite was applied;
// on any failure the original query is returned unchanged.
func (e *Enricher) RewriteQuery(ctx context.Context, query string) (string, bool) {
	if e.assistGen == nil || !e.assistRewrite || strings.TrimSpace(query) == "" {
		return query, false
	}

	ctx, cancel := context.WithTimeout(ctx, queryRewriteTimeout)
	defer cancel()

	out, err := e.assistGen.Generate(ctx, queryRewritePrompt+"\n\n"+query)
	if err != nil {
		slog.Warn("query rewrite failed; using original query", "error", err)
		return query, false
	}

	rewritten := sanitizeRewrittenQuery(out)
	if rewritten == "" || rewritten == query {
		return query, false
	}
	return rewritten, true
}

// sanitizeRewrittenQuery turns raw LLM output into something safe to hand to
// the search engine. Model output reaches a query parser, so leftovers matter:
// a rewrite wrapped in double quotes becomes a phrase query and matches
// nothing. Returns "" when nothing usable is left.
func sanitizeRewrittenQuery(out string) string {
	out = stripThinkBlocks(out)

	// First non-empty, non-fence line: models add reasoning or a code fence
	// around the actual query.
	var line string
	for _, candidate := range strings.Split(out, "\n") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || strings.HasPrefix(candidate, "```") {
			continue
		}
		line = candidate
		break
	}
	if line == "" {
		return ""
	}

	line = stripLabel(line)
	line = stripWrappingQuotes(line)
	line = stripLabel(line)

	// Any remaining quote characters would still be read as phrase delimiters.
	line = strings.Map(func(r rune) rune {
		switch r {
		case '"', '`':
			return ' '
		}
		return r
	}, line)

	line = strings.Join(strings.Fields(line), " ")
	if len(line) > maxRewrittenQueryLen {
		line = line[:maxRewrittenQueryLen]
		if idx := strings.LastIndex(line, " "); idx > 0 {
			line = line[:idx]
		}
	}
	return strings.TrimSpace(line)
}

// stripThinkBlocks removes <think>...</think> reasoning. An unterminated block
// swallows the rest of the output, which is what a truncated reasoning model
// produces.
func stripThinkBlocks(s string) string {
	for {
		open := strings.Index(strings.ToLower(s), "<think>")
		if open < 0 {
			return s
		}
		rest := s[open+len("<think>"):]
		closeIdx := strings.Index(strings.ToLower(rest), "</think>")
		if closeIdx < 0 {
			return s[:open]
		}
		s = s[:open] + rest[closeIdx+len("</think>"):]
	}
}

// stripLabel removes a leading "Query:"-style label.
func stripLabel(line string) string {
	lower := strings.ToLower(line)
	for _, prefix := range rewritePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return line
}

// stripWrappingQuotes removes matching surrounding quotes, repeatedly.
func stripWrappingQuotes(line string) string {
	for len(line) >= 2 {
		first, last := line[0], line[len(line)-1]
		if first != last {
			break
		}
		if first != '"' && first != '\'' && first != '`' {
			break
		}
		// Only strip when the quotes actually wrap the whole string, so a
		// query that legitimately contains a quoted phrase is left alone.
		if strings.ContainsRune(line[1:len(line)-1], rune(first)) {
			break
		}
		line = strings.TrimSpace(line[1 : len(line)-1])
	}
	return strings.TrimLeftFunc(line, func(r rune) bool {
		return unicode.IsSpace(r) || r == '*' || r == '#' || r == '-'
	})
}
