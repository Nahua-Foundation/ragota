// String and key helpers shared by the extractors: quoting, URL and
// placeholder handling, casing, and the contract join-key wrappers.
package ast

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"

	sitter "github.com/smacker/go-tree-sitter"
)

// unquote strips matching string delimiters and returns (value, true) if the
// text was a plain string literal. Handles ", ', ` and C#'s @" prefix.
func unquote(s string) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@") // C# verbatim
	if len(s) < 2 {
		return "", false
	}
	first, last := s[0], s[len(s)-1]
	if first != last || (first != '"' && first != '\'' && first != '`') {
		return "", false
	}
	body := s[1 : len(s)-1]
	if strings.ContainsRune(body, rune(first)) {
		return "", false
	}
	return body, true
}

// lastComponent is contract.LastComponent under its local name: the
// extractors compare the result against proto and framework tables verbatim,
// see the doc there for why surrounding whitespace is dropped.
func lastComponent(s string) string { return contract.LastComponent(s) }

// trimGenericArgs drops a type's generic argument list, keeping the type
// itself: IIntegrationEventHandler<OrderStarted> -> IIntegrationEventHandler,
// Repository<Order> -> Repository.
func trimGenericArgs(typ string) string {
	if i := strings.IndexByte(typ, '<'); i >= 0 {
		return strings.TrimSpace(typ[:i])
	}
	return strings.TrimSpace(typ)
}

// genericArgs returns the type arguments of a generic type reference, split at
// the top level so a nested generic stays one argument.
func genericArgs(typ string) []string {
	open := strings.IndexByte(typ, '<')
	shut := strings.LastIndexByte(typ, '>')
	if open < 0 || shut <= open {
		return nil
	}
	var out []string
	depth, start := 0, 0
	body := typ[open+1 : shut]
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '<', '[', '(':
			depth++
		case '>', ']', ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	if s := strings.TrimSpace(body[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}

// capitalizeFirst upper-cases a leading lowercase ASCII letter (createOrder ->
// CreateOrder). gRPC stubs expose lowerCamel method names in Java/TS while
// proto rpc_method units are PascalCase, and the linker compares method names
// exactly — so client extractors normalize before building the grpc key.
func capitalizeFirst(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// splitURL splits a URL/path expression into (host, path). Absolute URLs keep
// the host; bare paths return an empty host.
func splitURL(u string) (host, path string) {
	rest := u
	for _, scheme := range []string{"http://", "https://", "grpc://"} {
		if strings.HasPrefix(rest, scheme) {
			rest = rest[len(scheme):]
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				return rest[:i], rest[i:]
			}
			return rest, "/"
		}
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return "", rest
}

// printfVerb matches a printf/format verb in a format string: %s, %d, %+v,
// %#v, %-10.2f. "%%" is not a verb and stays.
var printfVerb = regexp.MustCompile(`%[-+# 0]*[0-9*]*(?:\.[0-9*]+)?[a-zA-Z]`)

// braceInterp matches a "${...}" interpolation: JS template literals, Spring
// property placeholders and shell/compose substitutions.
var braceInterp = regexp.MustCompile(`\$\{[^}]*\}`)

// placeholderize rewrites the interpolations inside one string literal to the
// "{}" route-parameter form. Python f-string fields ("{order_id}") already
// have that shape and pass through.
func placeholderize(lit string) string {
	lit = braceInterp.ReplaceAllString(lit, "{}")
	lit = printfVerb.ReplaceAllString(lit, "{}")
	return strings.ReplaceAll(lit, "%%", "%")
}

// interpolatedPath reduces a URL expression built from literals and runtime
// values to a route template, with every interpolated value replaced by a
// "{}" placeholder — the shape the linker's path matcher treats as a path
// parameter. It covers fmt.Sprintf and %-formatting, Python f-strings and
// .format(), JS template literals and plain concatenation.
//
// It reports false unless the result anchors on a real path: a leading "/",
// no whitespace, and at least one literal segment. That keeps log lines, SQL
// text and fully dynamic strings from being registered as routes.
func interpolatedPath(expr string) (string, bool) { return interpolatedTemplate(expr, false) }

// interpolatedTemplate is interpolatedPath with the choice of what a template
// that does not open on "/" means. relative says the target is written against
// a client's base address, where a literal first segment ("Users/" + id +
// "/Password") is part of the path and rooting it keeps the whole route;
// otherwise the leading run is read as the base URL itself and dropped.
func interpolatedTemplate(expr string, relative bool) (string, bool) {
	var b strings.Builder
	gap := false // a runtime value was seen since the last literal
	for i := 0; i < len(expr); {
		switch c := expr[i]; {
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for j < len(expr) && expr[j] != c {
				j++
			}
			if j >= len(expr) {
				return "", false // unterminated literal
			}
			if gap && b.Len() > 0 {
				b.WriteString("{}")
			}
			b.WriteString(placeholderize(expr[i+1 : j]))
			gap, i = false, j+1
		case contract.IsWordByte(c):
			for i < len(expr) && contract.IsWordByte(expr[i]) {
				i++
			}
			gap = true
		default:
			i++
		}
	}
	t := b.String()
	// A trailing value only forms its own segment when the literal ends on a
	// separator; otherwise it is glued into the last segment and unmatchable.
	if gap && strings.HasSuffix(t, "/") {
		t += "{}"
	}
	if i := strings.IndexAny(t, "?#"); i >= 0 {
		t = t[:i]
	}
	if !strings.Contains(t, "://") && !strings.HasPrefix(t, "/") {
		// Rooting the template asserts that its first segment is part of the
		// path, so the segment has to be spelled out — braces of any shape mean
		// a value stands there, and the value is the base address the rest is
		// written against. Python spells it f"{self._base_url}/watermark",
		// which named the base as a path segment until the test read braces
		// only in their reduced "{}" form.
		if head, _, sep := strings.Cut(t, "/"); relative && sep && !strings.ContainsAny(head, "{}") {
			t = "/" + t // a literal first segment is path, not base URL
		} else if i := strings.Index(t, "/"); i > 0 {
			t = t[i:] // drop a leading base-URL placeholder
		}
	}
	if strings.ContainsAny(t, " \t\n") {
		return "", false
	}
	if !strings.HasPrefix(t, "/") && !strings.Contains(t, "://") {
		return "", false
	}
	if !hasLiteralSegment(t) {
		return "", false // every segment is a placeholder
	}
	return t, true
}

// hasLiteralSegment reports whether a route template names at least one segment
// of its own. A template that is placeholders end to end ("/{}/{}") matches
// every route there is, which is the same as matching none.
func hasLiteralSegment(t string) bool {
	_, path := splitURL(t)
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg != "" && !strings.ContainsAny(seg, "{}") {
			return true
		}
	}
	return false
}

// splitPlaceholderDefault splits a "${KEY:default}" (Spring) or
// "${KEY:-default}" (shell, docker-compose) placeholder into its key and
// default value. ok is false when s is not a placeholder with a default.
func splitPlaceholderDefault(s string) (key, def string, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		return "", "", false
	}
	body := s[2 : len(s)-1]
	i := strings.Index(body, ":")
	if i < 0 {
		return body, "", false
	}
	return body[:i], strings.TrimPrefix(body[i+1:], "-"), true
}

// applyPlaceholderDefaults replaces every "${KEY:default}" in a configuration
// value with its default, which is the value the service runs with when the
// key is not set in the environment. Plain "${KEY}" references are left for
// the linker to resolve.
func applyPlaceholderDefaults(v string) string {
	return braceInterp.ReplaceAllStringFunc(v, func(m string) string {
		if _, def, ok := splitPlaceholderDefault(m); ok {
			return def
		}
		return m
	})
}

// tableName is the derivation the linker re-runs on the other side of the
// join; it lives in internal/contract so the two cannot drift.
func tableName(typeName string) string { return contract.TableName(typeName) }

// snakeCase converts CamelCase / PascalCase to snake_case, leaving text that
// is already snake_case untouched.
func snakeCase(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 && (isLowerOrDigit(s[i-1]) || (i+1 < len(s) && s[i+1] >= 'a' && s[i+1] <= 'z')) {
				b.WriteByte('_')
			}
			b.WriteByte(c - 'A' + 'a')
			continue
		}
		b.WriteByte(c)
	}
	return strings.Trim(b.String(), "_")
}

func isLowerOrDigit(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

// Contract join keys shared by the parsers. Construction and parsing live in
// internal/contract; these are thin local wrappers for the extractors.

// routeKey builds the join key for an HTTP route: "http:POST /a/b".
func routeKey(method, path string) string { return contract.HTTP(method, path) }

// topicKey builds the join key for a Kafka topic: "topic:orders.created".
func topicKey(name string) string { return contract.Topic(name) }

// grpcKey builds the join key for a gRPC method: "grpc:Service/Method" or
// "grpc:pkg.Service/Method". Empty service yields "grpc:/Method" which the
// linker matches by suffix.
func grpcKey(service, method string) string { return contract.GRPC(service, method) }

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// extractLineComments collects consecutive comment lines directly above a node.
func extractLineComments(content string, node *sitter.Node, prefix string) string {
	lines := strings.Split(content, "\n")
	startLine := int(node.StartPoint().Row)

	var comments []string
	for i := startLine - 1; i >= 0 && i >= startLine-10; i-- {
		if i < 0 || i >= len(lines) {
			break
		}
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, prefix) {
			comments = append([]string{strings.TrimPrefix(strings.TrimPrefix(line, prefix), " ")}, comments...)
		} else if line == "" {
			continue
		} else {
			break
		}
	}

	return strings.Join(comments, "\n")
}
