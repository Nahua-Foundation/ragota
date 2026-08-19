package graph

import (
	"fmt"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

func rpcUnit(id, repoID, qualified string) *domain.ASTUnit {
	return &domain.ASTUnit{ID: id, RepoID: repoID, Kind: store.KindRPCMethod, Qualified: qualified}
}

func TestMatchRPC(t *testing.T) {
	units := []*domain.ASTUnit{
		rpcUnit("1", "repoB", "grpc:orders.OrderService/CreateOrder"),
		rpcUnit("2", "repoB", "grpc:billing.BillingService/Charge"),
	}
	tests := []struct {
		name     string
		key      string
		wantID   string
		wantConf float32
	}{
		{"full key", "grpc:orders.OrderService/CreateOrder", "1", 0.95},
		{"key without package", "grpc:OrderService/CreateOrder", "1", 0.95},
		{"method only", "grpc:/CreateOrder", "1", 0.7},
		{"missing method", "grpc:OrderService/DeleteOrder", "", 0},
		{"wrong service falls back to method", "grpc:OtherService/Charge", "2", 0.7},
		{"not a grpc key", "http:GET /x", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, conf := matchRPC(units, tt.key)
			gotID := ""
			if u != nil {
				gotID = u.ID
			}
			if gotID != tt.wantID || conf != tt.wantConf {
				t.Errorf("matchRPC(%q) = (%q, %v), want (%q, %v)", tt.key, gotID, conf, tt.wantID, tt.wantConf)
			}
		})
	}
}

// TestMatchRPCAmbiguousSuffix pins the versioned-package case: a client key
// carries no proto package, so both orders.v1.OrderService and
// orders.v2.OrderService match by suffix and neither may win at ConfExact.
func TestMatchRPCAmbiguousSuffix(t *testing.T) {
	units := []*domain.ASTUnit{
		rpcUnit("1", "repoB", "grpc:orders.v1.OrderService/CreateOrder"),
		rpcUnit("2", "repoB", "grpc:orders.v2.OrderService/CreateOrder"),
	}
	u, conf := matchRPC(units, "grpc:OrderService/CreateOrder")
	if u == nil || conf != contract.ConfHeuristic {
		t.Fatalf("matchRPC ambiguous suffix = (%+v, %v), want a candidate at ConfHeuristic", u, conf)
	}
	cands, conf := rpcCandidates(buildRPCIndex(units), "grpc:OrderService/CreateOrder")
	if len(cands) != 2 || conf != contract.ConfHeuristic {
		t.Errorf("rpcCandidates = (%d cands, %v), want 2 candidates for disambiguation", len(cands), conf)
	}

	// An exact service match is not ambiguous, even next to a suffix match.
	u, conf = matchRPC(units, "grpc:orders.v2.OrderService/CreateOrder")
	if u == nil || u.ID != "2" || conf != contract.ConfExact {
		t.Errorf("matchRPC exact service = (%+v, %v), want unit 2 at ConfExact", u, conf)
	}
}

// TestMatchRPCSuffixWinsOverUnrelatedService: a single suffix match is still
// exact, and it outranks same-named methods of other services.
func TestMatchRPCSuffixBeatsMethodOnly(t *testing.T) {
	units := []*domain.ASTUnit{
		rpcUnit("other", "repoC", "grpc:billing.PaymentService/CreateOrder"),
		rpcUnit("match", "repoB", "grpc:orders.OrderService/CreateOrder"),
	}
	u, conf := matchRPC(units, "grpc:OrderService/CreateOrder")
	if u == nil || u.ID != "match" || conf != contract.ConfExact {
		t.Errorf("matchRPC = (%+v, %v), want unit match at ConfExact", u, conf)
	}
}

func routeUnit(id, repoID, qualified string) *domain.ASTUnit {
	return &domain.ASTUnit{ID: id, RepoID: repoID, Kind: store.KindHTTPRoute, Qualified: qualified}
}

func TestMatchRoute(t *testing.T) {
	tests := []struct {
		name    string
		units   []*domain.ASTUnit
		key     string
		srcRepo string
		wantID  string
	}{
		{
			name:    "exact match",
			units:   []*domain.ASTUnit{routeUnit("1", "b", "http:GET /api/users")},
			key:     "http:GET /api/users",
			srcRepo: "a",
			wantID:  "1",
		},
		{
			name:    "template segment {id}",
			units:   []*domain.ASTUnit{routeUnit("1", "b", "http:GET /users/{id}")},
			key:     "http:GET /users/123",
			srcRepo: "a",
			wantID:  "1",
		},
		{
			name:    "template segment :id",
			units:   []*domain.ASTUnit{routeUnit("1", "b", "http:GET /users/:id")},
			key:     "http:GET /users/42",
			srcRepo: "a",
			wantID:  "1",
		},
		{
			name:    "ANY method",
			units:   []*domain.ASTUnit{routeUnit("1", "b", "http:ANY /health")},
			key:     "http:POST /health",
			srcRepo: "a",
			wantID:  "1",
		},
		{
			name:    "segment count mismatch",
			units:   []*domain.ASTUnit{routeUnit("1", "b", "http:GET /api/users/{id}")},
			key:     "http:GET /api/users",
			srcRepo: "a",
			wantID:  "",
		},
		{
			name: "prefers cross-repo route",
			units: []*domain.ASTUnit{
				routeUnit("same", "a", "http:GET /api/users"),
				routeUnit("cross", "b", "http:GET /api/users"),
			},
			key:     "http:GET /api/users",
			srcRepo: "a",
			wantID:  "cross",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, conf := matchRoute(tt.units, tt.key, tt.srcRepo)
			gotID := ""
			if u != nil {
				gotID = u.ID
			}
			if gotID != tt.wantID {
				t.Errorf("matchRoute(%q) = (%q, %v), want id %q", tt.key, gotID, conf, tt.wantID)
			}
			if tt.wantID != "" && conf <= 0 {
				t.Errorf("matchRoute(%q) confidence = %v, want > 0", tt.key, conf)
			}
		})
	}
}

// TestMatchRouteQualityBeatsRepoPreference pins the monorepo case: an exact
// same-repo route must not lose to a fuzzier cross-repo one. The cross-repo
// preference is only a tie-break between equally good matches.
func TestMatchRouteQualityBeatsRepoPreference(t *testing.T) {
	units := []*domain.ASTUnit{
		routeUnit("same", "a", "http:GET /api/orders/42"),
		routeUnit("cross", "b", "http:GET /api/orders/{id}"),
	}
	u, conf := matchRoute(units, "http:GET /api/orders/42", "a")
	if u == nil || u.ID != "same" {
		t.Errorf("matchRoute = %+v, want the exact same-repo route", u)
	}
	if conf != contract.ConfExact {
		t.Errorf("matchRoute conf = %v, want ConfExact (no same-repo penalty)", conf)
	}

	// Equal quality: the cross-repo route wins, and the pair is not treated
	// as ambiguous because they differ in repo relationship.
	equal := []*domain.ASTUnit{
		routeUnit("same", "a", "http:GET /api/orders"),
		routeUnit("cross", "b", "http:GET /api/orders"),
	}
	cands := routeCandidates(buildRouteIndex(equal), "http:GET /api/orders", "a")
	if len(cands) != 2 || cands[0].unit.ID != "cross" {
		t.Fatalf("routeCandidates = %+v, want the cross-repo route first", cands)
	}
	if routesAmbiguous(cands) {
		t.Error("a same-repo and a cross-repo route are ordered by preference, not ambiguous")
	}
}

func TestRouteMatchScore(t *testing.T) {
	tests := []struct {
		name                                         string
		callMethod, callPath, routeMethod, routePath string
		want                                         float32
	}{
		{"exact", "GET", "/a/b", "GET", "/a/b", 0.95},
		{"method mismatch", "GET", "/a/b", "POST", "/a/b", 0},
		{"any call method", "ANY", "/a", "GET", "/a", 0.95 * 0.8},
		{"any route method", "DELETE", "/a", "ANY", "/a", 0.95 * 0.8},
		{"route param segment", "GET", "/a/123", "GET", "/a/{id}", 0.95 * 0.95},
		{"call param segment", "GET", "/a/{x}", "GET", "/a/b", 0.95 * 0.95},
		{"segment count mismatch", "GET", "/a/b/c", "GET", "/a/b", 0},
		{"literal mismatch", "GET", "/a/b", "GET", "/a/c", 0},
		{"case-insensitive segments", "GET", "/API/Users", "GET", "/api/users", 0.95},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := routeMatchScore(tt.callMethod, tt.callPath, tt.routeMethod, tt.routePath)
			if got != tt.want {
				t.Errorf("routeMatchScore(%q %q, %q %q) = %v, want %v",
					tt.callMethod, tt.callPath, tt.routeMethod, tt.routePath, got, tt.want)
			}
		})
	}
}

func tableUnit(id, repoID, qualified string) *domain.ASTUnit {
	return &domain.ASTUnit{ID: id, RepoID: repoID, Kind: store.KindDBTable, Qualified: qualified}
}

func TestMatchTable(t *testing.T) {
	units := []*domain.ASTUnit{
		tableUnit("other", "b", "db:orders"),
		tableUnit("same", "a", "db:orders"),
	}

	u, conf := matchTable(units, "db:orders", "a")
	if u == nil || u.ID != "same" || conf != contract.ConfExact {
		t.Errorf("matchTable same-repo = (%+v, %v), want (same, ConfExact)", u, conf)
	}

	// Two other repos declare the table: the pick is a guess, so it drops a
	// tier and becomes eligible for disambiguation.
	u, conf = matchTable(units, "db:orders", "c")
	if u == nil || conf != contract.ConfCrossFile {
		t.Errorf("matchTable ambiguous cross-repo = (%+v, %v), want ConfCrossFile", u, conf)
	}

	// A single cross-repo declaration is unambiguous.
	u, conf = matchTable(units[:1], "db:orders", "c")
	if u == nil || u.ID != "other" || conf != contract.ConfHigh {
		t.Errorf("matchTable single cross-repo = (%+v, %v), want (other, ConfHigh)", u, conf)
	}

	if u, _ := matchTable(units, "db:missing", "a"); u != nil {
		t.Errorf("matchTable(db:missing) = %+v, want nil", u)
	}
	if u, _ := matchTable(units, "orders", "a"); u != nil {
		t.Errorf("matchTable without db: prefix = %+v, want nil", u)
	}
}

// TestMatchTableKeepsSchema: schema-qualified tables are distinct tables, and
// a bare table name reaches them only at a weaker tier.
func TestMatchTableKeepsSchema(t *testing.T) {
	units := []*domain.ASTUnit{
		tableUnit("analytics", "b", "db:analytics.users"),
		tableUnit("public", "c", "db:public.users"),
	}

	u, conf := matchTable(units, "db:analytics.users", "a")
	if u == nil || u.ID != "analytics" || conf != contract.ConfHigh {
		t.Errorf("matchTable(db:analytics.users) = (%+v, %v), want (analytics, ConfHigh)", u, conf)
	}

	// "db:users" cannot say which schema it meant: weaker tier, and both
	// candidates are offered for disambiguation.
	cands, conf := tableCandidates(buildTableIndex(units), "db:users", "a")
	if len(cands) != 2 {
		t.Fatalf("tableCandidates(db:users) = %d candidates, want 2", len(cands))
	}
	if conf != contract.ConfHeuristic {
		t.Errorf("tableCandidates(db:users) conf = %v, want ConfHeuristic", conf)
	}

	// An exact key never loses to a schema-inferred one.
	withExact := append([]*domain.ASTUnit{tableUnit("exact", "b", "db:users")}, units...)
	u, conf = matchTable(withExact, "db:users", "a")
	if u == nil || u.ID != "exact" || conf != contract.ConfHigh {
		t.Errorf("matchTable exact over schema-inferred = (%+v, %v), want (exact, ConfHigh)", u, conf)
	}
}

// entityTableUnit is a db_table unit declared under an explicit name, carrying
// the entity it maps in its signature the way the ORM parsers publish it.
func entityTableUnit(id, repoID, qualified, entity string) *domain.ASTUnit {
	u := tableUnit(id, repoID, qualified)
	u.Signature = "entity:" + entity
	return u
}

// TestMatchTableThroughEntity: dotnet/eShop declares its table as
// ToTable("Catalog") on CatalogItem while the ORM detector keys the write it
// records db:catalog_items. The two halves join through the entity name.
func TestMatchTableThroughEntity(t *testing.T) {
	units := []*domain.ASTUnit{
		entityTableUnit("ef", "a", "db:catalog", "CatalogItem"),
	}

	u, conf := matchTable(units, "db:catalog_items", "a")
	if u == nil || u.ID != "ef" || conf != contract.ConfCrossFile {
		t.Errorf("matchTable(db:catalog_items) = (%+v, %v), want (ef, ConfCrossFile)", u, conf)
	}

	// The declared name still matches exactly, and outranks the entity detour.
	if u, conf := matchTable(units, "db:catalog", "a"); u == nil || conf != contract.ConfExact {
		t.Errorf("matchTable(db:catalog) = (%+v, %v), want (ef, ConfExact)", u, conf)
	}

	// An exact key never loses to an entity-derived match.
	withExact := append([]*domain.ASTUnit{tableUnit("exact", "a", "db:catalog_items")}, units...)
	if u, conf := matchTable(withExact, "db:catalog_items", "a"); u == nil || u.ID != "exact" || conf != contract.ConfExact {
		t.Errorf("matchTable exact over entity = (%+v, %v), want (exact, ConfExact)", u, conf)
	}

	// A table that names no entity is unreachable this way: the fallback is
	// inert until the parsers publish the signature.
	plain := []*domain.ASTUnit{tableUnit("plain", "a", "db:catalog")}
	if u, _ := matchTable(plain, "db:catalog_items", "a"); u != nil {
		t.Errorf("matchTable without entity signature = %+v, want nil", u)
	}
}

// TestMatchTableThroughEntityAmbiguous: several entities mapping to the same
// derived key are a guess, so they all reach disambiguation.
func TestMatchTableThroughEntityAmbiguous(t *testing.T) {
	units := []*domain.ASTUnit{
		entityTableUnit("catalog", "b", "db:catalog", "CatalogItem"),
		entityTableUnit("legacy", "c", "db:legacy_catalog", "CatalogItem"),
	}
	cands, conf := tableCandidates(buildTableIndex(units), "db:catalog_items", "a")
	if len(cands) != 2 {
		t.Fatalf("tableCandidates(db:catalog_items) = %d candidates, want 2", len(cands))
	}
	if conf != contract.ConfWeak {
		t.Errorf("tableCandidates(db:catalog_items) conf = %v, want ConfWeak", conf)
	}
	if conf > contract.ConfCrossFile {
		t.Errorf("conf = %v, want a tier the disambiguator is offered", conf)
	}
}

// TestEntityTableName pins the derivation to the one internal/index/ast
// applies, which is what produced the key on the edge side.
func TestEntityTableName(t *testing.T) {
	tests := []struct{ entity, want string }{
		{"CatalogItem", "catalog_items"},
		{"Order", "orders"},
		{"Address", "addresses"},
		{"Company", "companies"},
		{"Status", "statuses"},
		{"Box", "boxes"},
		{"Branch", "branches"},
		{"Day", "days"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := entityTableName(tt.entity); got != tt.want {
			t.Errorf("entityTableName(%q) = %q, want %q", tt.entity, got, tt.want)
		}
	}
}

func configUnit(id, repoID, qualified, name, value string) *domain.ASTUnit {
	return &domain.ASTUnit{
		ID: id, RepoID: repoID, Kind: store.KindConfigKey,
		Qualified: qualified, Name: name, Signature: value,
	}
}

func TestMatchConfigKey(t *testing.T) {
	t.Run("suffix match with normalization", func(t *testing.T) {
		// ORDERS_TOPIC must match kafka.orders-topic (normalized suffix).
		keys := []*domain.ASTUnit{
			configUnit("1", "a", "config:kafka.orders-topic", "orders-topic", "orders.created"),
		}
		if got := matchConfigKey(keys, "ORDERS_TOPIC", "a"); got != "orders.created" {
			t.Errorf("matchConfigKey(ORDERS_TOPIC) = %q, want orders.created", got)
		}
	})

	t.Run("prefers same repo", func(t *testing.T) {
		keys := []*domain.ASTUnit{
			configUnit("1", "other", "config:ORDERS_TOPIC", "ORDERS_TOPIC", "other.value"),
			configUnit("2", "mine", "config:ORDERS_TOPIC", "ORDERS_TOPIC", "mine.value"),
		}
		if got := matchConfigKey(keys, "ORDERS_TOPIC", "mine"); got != "mine.value" {
			t.Errorf("matchConfigKey same-repo = %q, want mine.value", got)
		}
	})

	t.Run("no match", func(t *testing.T) {
		keys := []*domain.ASTUnit{
			configUnit("1", "a", "config:db.host", "host", "localhost"),
		}
		if got := matchConfigKey(keys, "ORDERS_TOPIC", "a"); got != "" {
			t.Errorf("matchConfigKey(no match) = %q, want empty", got)
		}
	})

	t.Run("empty ref", func(t *testing.T) {
		if got := matchConfigKey(nil, "___", "a"); got != "" {
			t.Errorf("matchConfigKey(empty ref) = %q, want empty", got)
		}
	})

	// A one-word leaf ("topic") is too generic to answer ORDERS_TOPIC, even
	// from the referencing repo itself.
	t.Run("generic leaf does not match", func(t *testing.T) {
		keys := []*domain.ASTUnit{
			configUnit("1", "mine", "config:kafka.topic", "topic", "audit-events"),
		}
		if got := matchConfigKey(keys, "ORDERS_TOPIC", "mine"); got != "" {
			t.Errorf("matchConfigKey(kafka.topic) = %q, want empty", got)
		}
	})

	// A full-path match in a shared config repo outranks a leaf match at home.
	t.Run("full path outranks leaf", func(t *testing.T) {
		keys := []*domain.ASTUnit{
			configUnit("1", "mine", "config:kafka.orders.topic.name", "orders-topic", "wrong.value"),
			configUnit("2", "shared", "config:kafka.orders-topic", "orders-topic", "orders.created"),
		}
		if got := matchConfigKey(keys, "ORDERS_TOPIC", "mine"); got != "orders.created" {
			t.Errorf("matchConfigKey = %q, want orders.created from the full-path match", got)
		}
	})

	// Components must align: "orders-topics" is not "orders-topic".
	t.Run("component alignment", func(t *testing.T) {
		keys := []*domain.ASTUnit{
			configUnit("1", "a", "config:kafka.myorders-topic", "myorders-topic", "wrong.value"),
		}
		if got := matchConfigKey(keys, "ORDERS_TOPIC", "a"); got != "" {
			t.Errorf("matchConfigKey(myorders-topic) = %q, want empty", got)
		}
	})
}

// linearMatchConfigKey is the matcher as it read before the keys were indexed:
// a scan over every key, splitting its components per reference. It is kept
// here as the reference the bucketed index must answer identically to.
func linearMatchConfigKey(keys []*domain.ASTUnit, ref, repoID string) string {
	refComps := wordComponents(ref)
	if len(refComps) == 0 {
		return ""
	}
	var best *domain.ASTUnit
	bestScore := 0
	for _, k := range keys {
		path := wordComponents(contract.TrimKind(k.Qualified, contract.KindConfig))
		leaf := wordComponents(k.Name)
		tier := configNoMatch
		switch {
		case hasComponentSuffix(path, refComps):
			tier = configPathMatch
		case len(leaf) >= minLeafComponents && hasComponentSuffix(refComps, leaf):
			tier = configLeafMatch
		}
		if tier == configNoMatch {
			continue
		}
		score := tier * 2
		if k.RepoID == repoID {
			score++
		}
		if score > bestScore || (score == bestScore && best != nil && k.Qualified < best.Qualified) {
			best, bestScore = k, score
		}
	}
	if best == nil {
		return ""
	}
	return best.Signature
}

// TestConfigIndexMatchesLinearScan: bucketing by the trailing component is a
// data-structure change, so every reference must resolve to what the scan
// resolved it to — including the ones that resolve to nothing.
func TestConfigIndexMatchesLinearScan(t *testing.T) {
	prefixes := []string{"", "app.", "kafka.", "spring.kafka.", "legacy.module.orders."}
	middles := []string{"", "orders-", "orders-v2-", "payments-", "users-"}
	trailers := []string{"topic", "topics", "url", "name", "orders"}

	var keys []*domain.ASTUnit
	for i, p := range prefixes {
		for j, m := range middles {
			for k, tr := range trailers {
				leaf := m + tr
				keys = append(keys, configUnit(
					fmt.Sprintf("%d-%d-%d", i, j, k),
					fmt.Sprintf("repo%d", (i+j+k)%3),
					"config:"+p+leaf, leaf,
					fmt.Sprintf("value-%s%s", p, leaf),
				))
			}
		}
	}

	// A key whose Name is not the leaf of its path: only the leaf tier can
	// answer AUDIT_TOPIC, and only from the bucket of the leaf, not of the path.
	keys = append(keys, configUnit("aliased", "repo1", "config:app.kafka.settings", "audit-topic", "value-audit"))

	var refs []string
	for _, p := range []string{"", "KAFKA_", "SPRING_KAFKA_", "MODULE_ORDERS_"} {
		for _, m := range []string{"", "ORDERS_", "ORDERS_V2_", "PAYMENTS_", "MISSING_"} {
			for _, tr := range []string{"TOPIC", "TOPICS", "URL", "ORDERS", "PORT"} {
				refs = append(refs, p+m+tr)
			}
		}
	}
	refs = append(refs, "AUDIT_TOPIC", "___", "", "topic")

	idx := buildConfigIndex(keys)
	resolved := 0
	for _, repo := range []string{"repo0", "repo1", "repo2", "elsewhere"} {
		for _, ref := range refs {
			want := linearMatchConfigKey(keys, ref, repo)
			if want != "" {
				resolved++
			}
			if got := matchConfigKeyIndexed(idx, ref, repo); got != want {
				t.Errorf("matchConfigKeyIndexed(%q, %q) = %q, want %q (linear scan)",
					ref, repo, got, want)
			}
		}
	}
	if resolved < len(refs) {
		t.Errorf("only %d of %d lookups resolved to anything: the corpus is not exercising the matcher",
			resolved, len(refs)*4)
	}
	if got := matchConfigKeyIndexed(idx, "AUDIT_TOPIC", "repo1"); got != "value-audit" {
		t.Errorf("matchConfigKeyIndexed(AUDIT_TOPIC) = %q, want value-audit", got)
	}
}

// Corpus shape for the config-key scaling test and benchmark. The
// 12-repository corpus holds 1.76M config keys; a repository that resolves its
// topics from configuration — the supported way to name a topic — references
// them once per producer and consumer, so the reference count grows with the
// codebase too.
const (
	benchConfigKeys = 100_000
	benchConfigRefs = 1_000
)

// benchConfigTrailers keeps the trailing component of the corpus down to a
// small vocabulary, the way real configuration does (…url, …topic, …enabled):
// a corpus of keys with unique trailing words would flatter any index bucketed
// on it.
var benchConfigTrailers = []string{
	"topic", "url", "host", "port", "enabled", "timeout", "name", "key",
	"secret", "queue", "group", "region", "bucket", "path", "user", "password",
}

// configCorpus builds nKeys config keys spread over 12 repositories and nRefs
// references into them, together with the value each reference must resolve to.
func configCorpus(nKeys, nRefs int) (keys []*domain.ASTUnit, refs, want []string) {
	keys = make([]*domain.ASTUnit, nKeys)
	for i := range keys {
		leaf := fmt.Sprintf("setting%d-%s", i, benchConfigTrailers[i%len(benchConfigTrailers)])
		keys[i] = configUnit(
			fmt.Sprint(i),
			fmt.Sprintf("repo%d", i%12),
			fmt.Sprintf("config:app.module%d.%s", i%97, leaf),
			leaf,
			fmt.Sprintf("value-%d", i),
		)
	}
	step := nKeys / nRefs
	for i := 0; i < nRefs; i++ {
		n := i * step
		trailer := benchConfigTrailers[n%len(benchConfigTrailers)]
		refs = append(refs, fmt.Sprintf("SETTING%d_%s", n, trailer))
		want = append(want, fmt.Sprintf("value-%d", n))
	}
	return keys, refs, want
}

// TestConfigIndexScales: resolving many references must not cost a scan of
// every config key per reference.
//
// matchConfigKey indexes for a single lookup, so calling it once per reference
// is the shape the pass had before the index was hoisted; it is measured on a
// few references and extrapolated, since running all of them takes minutes.
// The bar is deliberately loose — the real gap is three orders of magnitude —
// so that a slow or loaded machine cannot fail the test, only a linear scan can.
func TestConfigIndexScales(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test over a 100k-key corpus")
	}
	keys, refs, want := configCorpus(benchConfigKeys, benchConfigRefs)

	const sample = 4
	start := time.Now()
	for i, ref := range refs[:sample] {
		if got := matchConfigKey(keys, ref, "repo0"); got != want[i] {
			t.Fatalf("matchConfigKey(%q) = %q, want %q", ref, got, want[i])
		}
	}
	perRef := time.Since(start) / sample * time.Duration(len(refs))

	start = time.Now()
	idx := buildConfigIndex(keys)
	for i, ref := range refs {
		if got := matchConfigKeyIndexed(idx, ref, "repo0"); got != want[i] {
			t.Fatalf("matchConfigKeyIndexed(%q) = %q, want %q", ref, got, want[i])
		}
	}
	indexed := time.Since(start)

	t.Logf("%d references over %d keys: shared index %v, per reference %v (extrapolated)",
		len(refs), len(keys), indexed, perRef)
	if indexed*10 > perRef {
		t.Errorf("shared index %v vs per reference %v: want at least 10x faster", indexed, perRef)
	}
}

func BenchmarkResolveConfigRefs(b *testing.B) {
	keys, refs, _ := configCorpus(benchConfigKeys, benchConfigRefs)

	// One index per reference: the cost the pass paid before it was hoisted.
	b.Run("per_reference", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for _, ref := range refs {
				matchConfigKey(keys, ref, "repo0")
			}
		}
	})
	b.Run("shared_index", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			idx := buildConfigIndex(keys)
			for _, ref := range refs {
				matchConfigKeyIndexed(idx, ref, "repo0")
			}
		}
	})
}

func TestWordComponents(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"ORDERS_TOPIC", []string{"orders", "topic"}},
		{"kafka.orders-topic", []string{"kafka", "orders", "topic"}},
		{"GetUserID", []string{"get", "user", "id"}},
		{"HTTPServer", []string{"http", "server"}},
		{"userId", []string{"user", "id"}},
		{"topic2", []string{"topic2"}},
		{"___", nil},
		{"", nil},
	}
	for _, tt := range tests {
		got := wordComponents(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("wordComponents(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("wordComponents(%q) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
}

func TestReceiverMatches(t *testing.T) {
	unit := &domain.ASTUnit{Name: "Save", Qualified: "repo.UserRepo.Save"}
	tests := []struct {
		recv string
		want bool
	}{
		{"UserRepo", true},
		{"userRepo", true},
		{"repo", true}, // a bare variable name still narrows nothing away
		{"OrderRepo", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := receiverMatches(unit, tt.recv); got != tt.want {
			t.Errorf("receiverMatches(%q) = %v, want %v", tt.recv, got, tt.want)
		}
	}
	// A package-level function has no owner to match.
	if receiverMatches(&domain.ASTUnit{Name: "Save", Qualified: "Save"}, "repo") {
		t.Error("an unqualified function must not match a receiver")
	}
}

func TestSplitGrpcKey(t *testing.T) {
	tests := []struct {
		in          string
		svc, method string
		ok          bool
	}{
		{"grpc:pkg.Svc/Method", "pkg.Svc", "Method", true},
		{"grpc:Svc/Method", "Svc", "Method", true},
		{"grpc:/Method", "", "Method", true},
		{"grpc:Method", "", "Method", true},
		{"grpc:Svc/", "Svc", "", false},
		{"grpc:", "", "", false},
		{"http:GET /x", "", "", false},
	}
	for _, tt := range tests {
		svc, method, ok := splitGrpcKey(tt.in)
		if svc != tt.svc || method != tt.method || ok != tt.ok {
			t.Errorf("splitGrpcKey(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.in, svc, method, ok, tt.svc, tt.method, tt.ok)
		}
	}
}

func TestSplitRouteKey(t *testing.T) {
	tests := []struct {
		in           string
		method, path string
		ok           bool
	}{
		{"http:GET /a/b", "GET", "/a/b", true},
		{"http:ANY /", "ANY", "/", true},
		{"http:GETnospace", "", "", false},
		{"grpc:Svc/M", "", "", false},
		{"http:", "", "", false},
	}
	for _, tt := range tests {
		method, path, ok := splitRouteKey(tt.in)
		if method != tt.method || path != tt.path || ok != tt.ok {
			t.Errorf("splitRouteKey(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.in, method, path, ok, tt.method, tt.path, tt.ok)
		}
	}
}

func TestIsPathParam(t *testing.T) {
	tests := []struct {
		seg  string
		want bool
	}{
		{"{id}", true},
		{":id", true},
		{"[id]", true},
		{"<id>", true},
		{"*", true},
		{"id", false},
		{"users", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isPathParam(tt.seg); got != tt.want {
			t.Errorf("isPathParam(%q) = %v, want %v", tt.seg, got, tt.want)
		}
	}
}
