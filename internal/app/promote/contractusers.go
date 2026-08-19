package promote

import (
	"context"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// The far side of a contract. contracts.go answers "where does this contract
// live" from the key a question spells out; this file answers "which code uses
// it" — who calls an rpc, who posts to a route, who publishes to a topic, who
// subscribes to one — and it has to work when the question spells nothing out.
//
// It is the same defect the rpc implementation lookup was built for, on the
// other side of the contract. "Which service calls the payment service Charge
// rpc" is answered by an `rpc_call grpc:PaymentService/Charge` edge that sits
// in the graph, resolved, on the expected line — but the callee is resolved by
// name, and the Node method that serves that rpc is called ChargeServiceHandler,
// so nothing matches and the edge is never consulted. "Which service subscribes
// to catalog product price changes" never names ProductPriceChangedIntegrationEvent
// at all. Neither question can be answered by extracting a key from it.
//
// So the match runs the other way round, from the edges to the question: read
// the repository's client-side contract edges and keep the ones whose key the
// question describes, component by component.
//
// Four things keep that from answering a different question than the one that
// was asked, and every one of them was put there by a measurement:
//
//   - Only the client side of a contract is read here. Promoting an
//     implementation for a "who calls it" question answers the opposite
//     question, exactly as promoting a caller for a "where does it live"
//     question put a load generator and a functional test above the handler.
//   - A key is accepted only when the part that distinguishes it from its
//     siblings is fully described. "The shipping service" names every rpc that
//     service has; promoting all of them puts a wrong line first.
//   - Nothing under a test path is promoted, and nothing under a load
//     generator'p. Both call the contract; neither is the code that uses it.
//   - The verb decides which end of a topic is meant, because "publishes" and
//     "subscribes to" name one key and have opposite answers.
//
// Placing a hit ahead of the ranked results asserts that it *is* the answer,
// which is a much stronger claim than ranking it highly — so a rule that only
// usually holds does not belong here.
const (
	// contractUseScanLimit bounds the client-side edges read per repository.
	contractUseScanLimit = 2000
	// maxPromotedContractUses caps how many use sites are placed ahead of the
	// text results for one query.
	maxPromotedContractUses = 10
)

// A topic question has to say which end of the contract it wants: "which
// service publishes X" and "which service subscribes to X" name the same key
// and have opposite answers, and the verb is the only thing that separates
// them. The call side needs no such test — the opposite of "who calls this rpc"
// is "where is it implemented", which is a different lookup (contracts.go) —
// so it is selected by the callers intent itself.
var (
	consumeVerbRe = regexp.MustCompile(`(?i)\b(?:consumes?|consumers?|subscribes?|subscribers?|listens?|receives?|reacts?|handles?|processes)\b`)
	produceVerbRe = regexp.MustCompile(`(?i)\b(?:publishes?|publishers?|produces?|producers?|emits?|dispatches?|raises?|fires?)\b`)
)

// contractUseKinds picks the edges the question is asking about.
func contractUseKinds(query string, callers bool) []string {
	var kinds []string
	if callers {
		kinds = append(kinds, store.EdgeRPCCall, store.EdgeHTTPCall)
	}
	if consumeVerbRe.MatchString(query) {
		kinds = append(kinds, store.EdgeConsumes)
	}
	if produceVerbRe.MatchString(query) {
		kinds = append(kinds, store.EdgeProduces)
	}
	return kinds
}

// PromoteContractUses places the code that uses the contract a question
// describes ahead of the ranked text hits. It returns hits unchanged when the
// question asks for no side of any contract, or when no key it describes is in
// the graph.
func (p *Promoter) PromoteContractUses(ctx context.Context, q *index.SearchQuery, intent string,
	hits []*index.Hit, meta map[string]interface{}) []*index.Hit {

	if p.store == nil {
		return hits
	}
	kinds := contractUseKinds(q.Query, intent == IntentCallers)
	if len(kinds) == 0 {
		return hits
	}
	words := questionWords(q.Query)
	if len(words) == 0 {
		return hits
	}
	verbs := httpVerbsNamed(q.Query)

	repos := q.Repos
	if len(repos) == 0 {
		repos = []string{""}
	}
	var edges []*domain.Edge
	score := map[*domain.Edge]int{}
	for _, repoID := range repos {
		found, err := p.store.GetEdges(ctx, domain.QueryOpts{
			RepoID: repoID, Kinds: kinds, Limit: contractUseScanLimit,
		})
		if err != nil {
			slog.Debug("contract use intent: edge lookup failed", "repo", repoID, "error", err)
			continue
		}
		for _, e := range found {
			if e == nil || testishPath(e.FilePath) || syntheticTrafficPath(e.FilePath) {
				continue
			}
			if !repoAllowed(q.Repos, e.RepoID) {
				continue
			}
			n, ok := keyDescribed(e.DstName, words, verbs)
			if !ok {
				continue
			}
			edges = append(edges, e)
			score[e] = n
		}
	}
	if len(edges) == 0 {
		return hits
	}
	// The most completely described key first — a question that named the rpc's
	// service as well as its method means that one, not a sibling that matched
	// the method alone — then the shared edge order: production code before
	// test scaffolding, resolved destinations before name-match guesses,
	// confidence, and location so equal edges have a fixed order.
	sortEdgesBy(edges, kindRank)
	sort.SliceStable(edges, func(i, j int) bool { return score[edges[i]] > score[edges[j]] })

	srcIDs := make([]string, 0, len(edges))
	for _, e := range edges {
		srcIDs = append(srcIDs, e.SrcID)
	}
	srcUnits := p.unitsByID(ctx, srcIDs)

	filters := index.ParseFilters(q.Filter)
	promoted := make([]*index.Hit, 0, len(edges))
	seen := map[string]bool{}
	keys := map[string]bool{}
	for _, e := range edges {
		if len(promoted) >= maxPromotedContractUses {
			break
		}
		at := e.RepoID + "\x00" + e.FilePath + "\x00" + strconv.Itoa(e.Line)
		if seen[at] {
			continue
		}
		verb := callerKindVerb[e.Kind]
		if verb == "" {
			verb = "uses"
		}
		hit := contractEdgeHit(e, srcUnits[e.SrcID], e.DstName, verb)
		if !filters.Empty() && !filters.Match(hit.Language, hit.Kind, hit.FilePath) {
			continue
		}
		seen[at] = true
		keys[e.DstName] = true
		promoted = append(promoted, hit)
	}
	if len(promoted) == 0 {
		return hits
	}
	meta["contract_use_keys"] = slices.Sorted(maps.Keys(keys))
	meta["contract_uses"] = len(promoted)
	return prependPromoted(promoted, hits)
}

// keyDescribed reports whether the question describes this contract key, and
// how much of it.
//
// A key is accepted only when the part that distinguishes it from its siblings
// is described in full: the rpc's method, the route's path, the topic's name.
// The rest of the key is what the contract shares with its neighbours — every
// rpc of one service carries that service's name — so a question that names
// only that names a dozen contracts at once, and promoting all of them puts a
// wrong line first. The count returned is key-wide, so a question that named
// the service as well as the method outranks one that matched on the method.
//
// Being described in full is not enough when there is only one word to
// describe. Measured, that is where every false promotion came from: `Create`,
// `Sync`, `GET /owners`, `GET /product/{}` are one common word each, and a
// question that happens to contain that word has not named a contract — it put
// a load generator above "where are the catalog entries read from the local
// json file" and four unrelated `Create` rpcs above a grafana callers
// question. A single-word rpc method therefore needs its service named too,
// and a route needs a second path segment.
func keyDescribed(key string, words map[string]bool, httpVerbs map[string]bool) (int, bool) {
	switch {
	case contract.IsKind(key, contract.KindGRPC):
		service, method, ok := contract.ParseGRPC(key)
		if !ok {
			return 0, false
		}
		n, all := fullyDescribed(method, words)
		if !all {
			return 0, false
		}
		// The package a proto declares its service in is a namespace no
		// question repeats: "hipstershop.PaymentService" is the payment
		// app.
		svc, svcAll := fullyDescribed(service[strings.LastIndex(service, ".")+1:], words)
		if n < 2 && !svcAll {
			return 0, false
		}
		return n + svc, true

	case contract.IsKind(key, contract.KindHTTP):
		method, path, ok := contract.ParseHTTP(key)
		if !ok {
			return 0, false
		}
		// A question that says which verb it means says it about this route:
		// "which service posts an order" is not answered by a GET.
		if len(httpVerbs) > 0 && !httpVerbs[strings.ToUpper(method)] {
			return 0, false
		}
		n, all := fullyDescribed(trimPathParams(path), words)
		if !all || n < 2 {
			return 0, false
		}
		return n, true

	case contract.IsKind(key, contract.KindTopic):
		// An unresolved "${ORDERS_TOPIC}" reference names a config entry, not a
		// topic: there is nothing for a question to have described.
		if _, isRef := contract.ParseTopicRef(key); isRef {
			return 0, false
		}
		// One word is enough here, and only here: a topic's name is the whole
		// contract, so "the orders queue" names exactly one of them, while
		// "create" names one method of many and "/owners" one path of many.
		name, _ := contract.ParseTopic(key)
		n, all := fullyDescribed(name, words)
		if !all {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// fullyDescribed reports how many distinct content components of name the
// question describes, and whether that is all of them.
//
// The scaffolding is dropped from both sides or the comparison is asymmetric:
// "the grpc shipping service" leaves "shipping", while the key it names,
// ShippingService, still carries the component "service".
func fullyDescribed(name string, words map[string]bool) (int, bool) {
	var described, content int
	seen := map[string]bool{}
	for _, raw := range graph.WordComponents(name) {
		c, ok := contentComponent(raw)
		if !ok || seen[c] {
			continue
		}
		seen[c] = true
		content++
		if words[c] {
			described++
		}
	}
	return described, content > 0 && described == content
}

// syntheticTrafficPath reports whether a path looks like a load generator, by
// the naming conventions the tools ship with.
//
// It is the other half of the guard testishPath is: a load generator calls the
// same contracts production code does — "GET /api/catalogue/products" has
// exactly one caller in the graph and it is `load-gen/robot-shop.py` — and
// promoting it says that synthetic traffic is the code that uses this
// contract. The same distinction had to be drawn by hand when the query set
// was written, and it is what kept locustfile.py from being an answer.
func syntheticTrafficPath(path string) bool {
	p := strings.ToLower(path)
	for _, marker := range []string{
		"locustfile", "loadgen", "load-gen", "load_gen",
		"loadtest", "load-test", "load_test",
		"/benchmarks/", "/gatling/", "/jmeter/",
	} {
		if strings.Contains(p, marker) {
			return true
		}
	}
	return false
}

// trimPathParams drops a route path's parameter segments — "{id}", ":id",
// "*rest". A parameter's name is per-request data rather than part of what the
// route is, which is the same reason the key drops the query string, and a
// question never has to name one to be asking about that route.
func trimPathParams(path string) string {
	var b strings.Builder
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || strings.HasPrefix(seg, "{") || strings.HasPrefix(seg, ":") || strings.HasPrefix(seg, "*") {
			continue
		}
		b.WriteByte('/')
		b.WriteString(seg)
	}
	return b.String()
}

// questionWords reduces a question to the vocabulary its contract keys are
// matched against.
func questionWords(query string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range graph.WordComponents(query) {
		if c, ok := contentComponent(raw); ok {
			out[c] = true
		}
	}
	return out
}

// contentComponent normalizes one word component and reports whether it says
// anything about *which* contract is meant.
func contentComponent(w string) (string, bool) {
	stem := stemWord(strings.ToLower(w))
	if contractScaffolding[stem] || apiVersionRe.MatchString(w) {
		return "", false
	}
	return stem, true
}

// contractScaffolding are the words a question and a contract key are both
// built out of. They are the grammar of naming a contract, never the name of
// one: dropping them from both sides is what lets "the grpc basket service"
// describe BasketApi.Basket, and keeping them would make the comparison
// asymmetric. A key made of nothing else — a queue literally called "events" —
// is left to retrieval, because fullyDescribed requires content.
//
// Stored stemmed, because that is the form components are compared in.
var contractScaffolding = stemSet(
	// naming a contract
	"grpc", "rpc", "http", "api", "service", "server", "client", "method",
	"handler", "call", "endpoint", "route", "topic", "queue", "channel",
	"message", "event", "integration", "command", "request", "response",
	// naming the code around it
	"code", "class", "project", "module", "package", "function",
	// question grammar
	"the", "a", "an", "which", "what", "who", "where", "when", "how",
	"is", "are", "do", "does", "did", "to", "from", "in", "of", "for",
	"and", "or", "its", "it", "this", "that", "with", "on", "at", "by",
)

// apiVersionRe matches a path's version segment ("v1", "v2"), which no
// question has to repeat to be about that route.
var apiVersionRe = regexp.MustCompile(`(?i)^v\d+$`)

// httpVerbRe finds the request methods a question names.
var httpVerbRe = regexp.MustCompile(`(?i)\b(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)s?\b`)

// httpVerbsNamed returns the request methods the question names, upper-cased,
// or an empty set when it names none.
func httpVerbsNamed(query string) map[string]bool {
	var out map[string]bool
	for _, m := range httpVerbRe.FindAllStringSubmatch(query, -1) {
		if out == nil {
			out = map[string]bool{}
		}
		out[strings.ToUpper(m[1])] = true
	}
	return out
}

// stemWord reduces an English word to a rough stem, so that a question can
// describe a key it does not spell the same way: "product price changes" has
// to reach ProductPriceChangedIntegrationEvent. Both sides go through it, so
// the comparison stays symmetric — it can only collapse two spellings into
// one, never tell two apart that were the same.
//
// It is deliberately not a real stemmer. Under-collapsing costs a match that
// retrieval still gets a chance at; over-collapsing puts a wrong line first.
func stemWord(w string) string {
	if len(w) < 5 {
		return w
	}
	stem := w
	switch {
	case strings.HasSuffix(w, "ies"):
		stem = w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "ing"):
		stem = w[:len(w)-3]
	case strings.HasSuffix(w, "ed"):
		stem = w[:len(w)-2]
	case strings.HasSuffix(w, "ss"): // "address" is not a plural
	case strings.HasSuffix(w, "es"):
		stem = w[:len(w)-2]
	case strings.HasSuffix(w, "s"):
		stem = w[:len(w)-1]
	}
	// "price" and "prices" have to meet somewhere, and "pric" is where.
	stem = strings.TrimSuffix(stem, "e")
	if len(stem) < 4 {
		return w
	}
	return stem
}

func stemSet(words ...string) map[string]bool {
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[stemWord(w)] = true
	}
	return out
}
