// Package contract defines the typed contract join keys shared by the AST
// parsers (key construction) and the graph linker (key parsing), plus the
// named confidence tiers used when scoring linked edges.
//
// Keys are plain strings with a kind prefix:
//
//	grpc:<pkg>.<Svc>/<Method>
//	http:<METHOD> <path>
//	topic:<name>
//	topic:${REF}       (unresolved config reference)
//	db:<table>
//	config:<dot.path>
//
// Constructors normalize their inputs (see HTTP); parsers accept exactly what
// the constructors produce plus the historical liberal forms (e.g. a gRPC key
// without a service: "grpc:/Method" or "grpc:Method").
package contract

import "strings"

// Kind identifies a contract key namespace (the part before the first ':').
type Kind string

// Contract key kinds.
const (
	KindGRPC   Kind = "grpc"
	KindHTTP   Kind = "http"
	KindTopic  Kind = "topic"
	KindDB     Kind = "db"
	KindConfig Kind = "config"
)

// prefix returns the literal key prefix for the kind, e.g. "grpc:".
func (k Kind) prefix() string { return string(k) + ":" }

// topicRefPrefix opens an unresolved config reference: "topic:${REF}".
const topicRefPrefix = "topic:${"

// ---------------------------------------------------------------------------
// Constructors
// ---------------------------------------------------------------------------

// GRPC builds the join key for a gRPC method: "grpc:Service/Method" or
// "grpc:pkg.Service/Method". An empty service yields "grpc:/Method", which
// the linker matches by method suffix.
func GRPC(service, method string) string {
	return string(KindGRPC) + ":" + service + "/" + method
}

// HTTP builds the join key for an HTTP route: "http:POST /a/b".
// The method is upper-cased (empty means "ANY") and the path is normalized
// to a single leading slash with no trailing slash. A query string or fragment
// is cut off: it is per-request data that no server route declares, so keeping
// it would make the key unjoinable ("/api/orders?status=new").
//
// Every path parameter is reduced to "{}", whatever syntax declared it and
// whatever it was called. A parameter's name is local to the side that wrote
// it — Express says "/check/:id" where the client that calls it says
// "/check/{id}" and Flask says "/check/<int:id>" — and none of those spellings
// is the contract. Reducing them makes the stored key agree with what route
// matching has always meant: routeMatchScoreSegs treats a parameter segment on
// either side as matching the other, so before this the two sides linked and
// yet an exact lookup of the key found nothing. The readable form survives on
// the unit itself, in Name and Signature.
func HTTP(method, path string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = "ANY"
	}
	path = "/" + strings.Trim(TrimQuery(strings.TrimSpace(path)), "/")
	return string(KindHTTP) + ":" + method + " " + ReducePath(path)
}

// ReducePath rewrites every path parameter to "{}", leaving static text alone:
// "/pets/{petId}/visits/:visitId" -> "/pets/{}/visits/{}".
//
// Reduction is per parameter, not per segment, so a segment that mixes
// literals with parameters keeps its shape: "/users/{id}N{tenant}/password"
// reduces to "/users/{}N{}/password" and stays distinguishable from
// "/users/{id}/password". The whole-segment syntaxes (":id", "<int:id>",
// "[id]", "*") have no literal part to keep by construction.
func ReducePath(path string) string {
	if !strings.ContainsAny(path, "{:<[*") {
		return path
	}
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		segs[i] = reduceSegment(seg)
	}
	return strings.Join(segs, "/")
}

// reduceSegment reduces the parameters of one path segment.
func reduceSegment(seg string) string {
	if seg == "" {
		return seg
	}
	switch seg[0] {
	case ':', '<', '[':
		return "{}"
	}
	if seg == "*" || seg == "**" {
		return "{}"
	}
	if !strings.Contains(seg, "{") {
		return seg
	}
	// Brace groups may nest a pattern that itself contains braces or
	// brackets ("{id:[0-9]{2}}"), so the scan counts depth rather than
	// looking for the next '}'.
	var b strings.Builder
	depth := 0
	for _, r := range seg {
		switch {
		case r == '{':
			if depth == 0 {
				b.WriteString("{}")
			}
			depth++
		case r == '}':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IsPathParam reports whether a path segment is a template parameter rather
// than a literal: "{id}", "{id:[0-9]+}", ":id", "<int:id>", "[id]" and the
// "*" / "**" wildcards, which is the union of what the frameworks in this
// corpus write. It answers about the segment as a whole, which is what route
// matching needs; ReducePath is the finer-grained operation.
func IsPathParam(seg string) bool {
	if seg == "" {
		return false
	}
	switch seg[0] {
	case '{', ':', '[', '<':
		return true
	}
	return seg == "*" || seg == "**"
}

// TrimQuery cuts a path at the first '?' or '#'.
func TrimQuery(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		return path[:i]
	}
	return path
}

// Topic builds the join key for a Kafka topic: "topic:orders.created".
func Topic(name string) string { return string(KindTopic) + ":" + name }

// TopicRef builds the key for a topic known only as a config reference:
// "topic:${ORDERS_TOPIC}". The linker later rewrites it to Topic(value).
func TopicRef(ref string) string { return topicRefPrefix + ref + "}" }

// DB builds the join key for a database table: "db:orders".
func DB(table string) string { return string(KindDB) + ":" + table }

// Config builds the qualified key for a configuration entry: "config:kafka.topic".
func Config(path string) string { return string(KindConfig) + ":" + path }

// ---------------------------------------------------------------------------
// Parsers
// ---------------------------------------------------------------------------

// ParseGRPC splits "grpc:[pkg.Svc]/Method" into service and method.
// The service may be empty ("grpc:/Method" or the slashless "grpc:Method").
// ok is false when the key is not a gRPC key or the method is empty; the
// service component is still returned in the latter case.
func ParseGRPC(key string) (service, method string, ok bool) {
	rest, found := strings.CutPrefix(key, KindGRPC.prefix())
	if !found {
		return "", "", false
	}
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return "", rest, rest != ""
	}
	return rest[:i], rest[i+1:], rest[i+1:] != ""
}

// ParseHTTP splits "http:METHOD /path" into method and path. ok is false
// when the key is not an HTTP key or lacks the space separator. A query string
// or fragment is cut off here as well, so keys stored before HTTP started
// normalizing them still join.
func ParseHTTP(key string) (method, path string, ok bool) {
	rest, found := strings.CutPrefix(key, KindHTTP.prefix())
	if !found {
		return "", "", false
	}
	parts := strings.SplitN(rest, " ", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], TrimQuery(parts[1]), true
}

// ParseTopic strips the "topic:" prefix. Note that an unresolved reference
// "topic:${REF}" parses as the literal name "${REF}"; check ParseTopicRef
// first when references must be treated specially.
func ParseTopic(key string) (name string, ok bool) {
	return cutPrefix(key, KindTopic)
}

// ParseTopicRef extracts REF from an unresolved reference "topic:${REF}".
func ParseTopicRef(key string) (ref string, ok bool) {
	if !strings.HasPrefix(key, topicRefPrefix) || !strings.HasSuffix(key, "}") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(key, topicRefPrefix), "}"), true
}

// ParseDB strips the "db:" prefix.
func ParseDB(key string) (table string, ok bool) {
	return cutPrefix(key, KindDB)
}

// ParseConfig strips the "config:" prefix.
func ParseConfig(key string) (path string, ok bool) {
	return cutPrefix(key, KindConfig)
}

// cutPrefix strips the kind's prefix, returning ("", false) — not the
// original key — when key is not of that kind.
func cutPrefix(key string, k Kind) (string, bool) {
	rest, found := strings.CutPrefix(key, k.prefix())
	if !found {
		return "", false
	}
	return rest, true
}

// IsKind reports whether key belongs to the given kind's namespace.
func IsKind(key string, k Kind) bool { return strings.HasPrefix(key, k.prefix()) }

// TrimKind strips the kind's prefix from key, returning key unchanged when it
// is not of that kind. Useful for display labels where a foreign key should
// pass through as-is (mirrors strings.TrimPrefix semantics).
func TrimKind(key string, k Kind) string { return strings.TrimPrefix(key, k.prefix()) }

// TableName derives the table an ORM maps an entity to when the mapping names
// no table explicitly: snake_case plus a naive English plural.
//
// It is one function because it is one join. The indexer derives the key it
// stores on a db_table unit from the entity name, and the linker re-derives the
// same key from the entity name recorded in that unit's signature; two
// derivations that disagree silently un-join every ORM write from the table it
// was declared against. They did disagree, on entity names carrying an
// underscore or a package qualifier: "User_Profile" was stored as
// user__profiles and looked up as user_profiles, and "models.User" was stored
// as users and looked up as models_users. Neither pair ever met.
//
// The qualifier is dropped (a package path is not part of the table name) and
// the rest goes through the shared word decomposition, so separators of every
// spelling collapse to one underscore.
func TableName(entity string) string {
	name := strings.TrimLeft(strings.TrimSpace(entity), "*&")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	s := strings.Join(WordComponents(name), "_")
	if s == "" {
		return ""
	}
	switch {
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "z"),
		strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	case strings.HasSuffix(s, "y") && len(s) > 1 && !isVowel(s[len(s)-2]):
		return s[:len(s)-1] + "ies"
	default:
		return s + "s"
	}
}

func isVowel(b byte) bool {
	return b == 'a' || b == 'e' || b == 'i' || b == 'o' || b == 'u'
}
