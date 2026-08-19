package graph

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
	"github.com/Nahua-Foundation/ragota/internal/store/sqlite"
)

func TestExprMatches(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		tracked []string
		want    bool
	}{
		{"case and underscore insensitive", "UserId", []string{"user_id"}, true},
		{"getter expression contains ident", "req.GetUserId()", []string{"user_id"}, true},
		{"struct field access", "order.UserID", []string{"user_id"}, true},
		{"no match", "amount", []string{"user_id"}, false},
		{"short ident exact match", "id", []string{"id"}, true},
		{"short ident no substring match", "someid", []string{"id"}, false},
		{"empty expr", "", []string{"user_id"}, false},
		{"empty tracked ident", "x", []string{""}, false},
		{"no tracked", "x", nil, false},
		// Token boundaries: a tracked identifier must align with whole word
		// components and end a token, or every neighbouring field matches.
		{"whole token", "user", []string{"user"}, true},
		{"longer word is not the tracked one", "username", []string{"user"}, false},
		{"leading component is not a match", "user_agent", []string{"user"}, false},
		{"camel leading component", "userAgent", []string{"user"}, false},
		{"trailing component matches", "user_agent", []string{"agent"}, true},
		{"getter of the tracked value", "getUser()", []string{"user"}, true},
		{"partial component", "users", []string{"user"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exprMatches(tt.expr, tt.tracked); got != tt.want {
				t.Errorf("exprMatches(%q, %v) = %v, want %v", tt.expr, tt.tracked, got, tt.want)
			}
		})
	}
}

func TestMatchingFields(t *testing.T) {
	fields := map[string]string{
		"user_id": "req.UserID",
		"amount":  "order.Amount",
		"email":   "user.Email",
	}
	got := matchingFields(fields, []string{"userID"})
	if len(got) != 1 || got[0] != "user_id" {
		t.Errorf("matchingFields = %v, want [user_id]", got)
	}

	if got := matchingFields(nil, []string{"userID"}); got != nil {
		t.Errorf("matchingFields(nil) = %v, want nil", got)
	}
	if got := matchingFields(fields, []string{"missing"}); got != nil {
		t.Errorf("matchingFields(no match) = %v, want nil", got)
	}

	// Multiple matches come back sorted.
	multi := map[string]string{"b_field": "userID", "a_field": "userID"}
	got = matchingFields(multi, []string{"userID"})
	if len(got) != 2 || got[0] != "a_field" || got[1] != "b_field" {
		t.Errorf("matchingFields multi = %v, want [a_field b_field]", got)
	}
}

func TestMatchingArg(t *testing.T) {
	if got := matchingArg([]string{"ctx", "userID", "amount"}, []string{"user_id"}); got != 1 {
		t.Errorf("matchingArg = %d, want 1", got)
	}
	if got := matchingArg([]string{"ctx", "amount"}, []string{"user_id"}); got != -1 {
		t.Errorf("matchingArg no match = %d, want -1", got)
	}
	if got := matchingArg(nil, []string{"user_id"}); got != -1 {
		t.Errorf("matchingArg(nil) = %d, want -1", got)
	}
}

func step(unitID, via string) *TraceStep {
	return &TraceStep{Unit: &domain.ASTUnit{ID: unitID}, Via: via}
}

func TestBetterChain(t *testing.T) {
	crossing := []*TraceStep{step("1", ""), step("2", store.EdgeRPCCall), step("3", store.EdgeKafkaFlow)}
	local := []*TraceStep{step("1", ""), step("4", store.EdgeCall), step("5", store.EdgeCall), step("6", store.EdgeCall)}
	short := []*TraceStep{step("1", ""), step("7", store.EdgeCall)}

	if !betterChain(crossing, local) {
		t.Error("chain with more service crossings should win over a longer local chain")
	}
	if betterChain(local, crossing) {
		t.Error("local chain should not beat a crossing chain")
	}
	if !betterChain(local, short) {
		t.Error("with equal crossings the longer chain should win")
	}
	if betterChain(short, local) {
		t.Error("shorter chain should not beat a longer one at equal crossings")
	}
	if !betterChain(short, nil) {
		t.Error("any chain beats nil")
	}
}

func TestIsPrefixChain(t *testing.T) {
	full := []*TraceStep{step("1", ""), step("2", ""), step("3", "")}
	prefix := []*TraceStep{step("1", ""), step("2", "")}
	diverging := []*TraceStep{step("1", ""), step("9", "")}
	longer := []*TraceStep{step("1", ""), step("2", ""), step("3", ""), step("4", "")}

	if !isPrefixChain(prefix, full) {
		t.Error("prefix should be detected")
	}
	if !isPrefixChain(full, full) {
		t.Error("a chain is a prefix of itself")
	}
	if isPrefixChain(diverging, full) {
		t.Error("diverging chain is not a prefix")
	}
	if isPrefixChain(longer, full) {
		t.Error("longer chain is not a prefix of a shorter one")
	}
}

func TestNormAll(t *testing.T) {
	got := normAll([]string{"User_Id", "B", "amount"})
	want := []string{"amount", "b", "userid"}
	if len(got) != len(want) {
		t.Fatalf("normAll = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normAll = %v, want %v", got, want)
		}
	}
	if got := normAll(nil); len(got) != 0 {
		t.Errorf("normAll(nil) = %v, want empty", got)
	}
}

// --- integration tests on a sqlite store ---

func openTestStore(t *testing.T) *sqlite.SQLite {
	t.Helper()
	st, err := sqlite.Open(&sqlite.Config{Path: filepath.Join(t.TempDir(), "test.db"), PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func storeFunc(t *testing.T, st *sqlite.SQLite, repoID, name, signature string) *domain.ASTUnit {
	t.Helper()
	u := &domain.ASTUnit{
		RepoID: repoID, FilePath: "src/" + name + ".go", Language: "go",
		Kind: "function", Name: name, Qualified: "pkg." + name,
		Signature: signature,
	}
	if err := st.StoreASTUnit(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func storeUnit(t *testing.T, st *sqlite.SQLite, u *domain.ASTUnit) *domain.ASTUnit {
	t.Helper()
	if err := st.StoreASTUnit(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func storeEdge(t *testing.T, st *sqlite.SQLite, e *domain.Edge) *domain.Edge {
	t.Helper()
	if e.Confidence == 0 {
		e.Confidence = 1.0
	}
	if err := st.StoreEdge(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e
}

func callMeta(t *testing.T, args ...string) string {
	t.Helper()
	b, err := json.Marshal(&store.EdgeMeta{Args: args})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTraceFollowsCallEdge(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	a := storeFunc(t, st, "r1", "A", "(userID string)")
	b := storeFunc(t, st, "r1", "B", "(uid string)")
	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: a.ID, DstID: b.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "B", Line: 12,
		Meta: callMeta(t, "userID"),
	})

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "A", Param: "userID"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chains != 1 {
		t.Errorf("Chains = %d, want 1", res.Chains)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2: %+v", len(res.Steps), res.Steps)
	}
	if res.Steps[0].Unit.ID != a.ID || res.Steps[1].Unit.ID != b.ID {
		t.Errorf("chain units = %s -> %s, want %s -> %s",
			res.Steps[0].Unit.ID, res.Steps[1].Unit.ID, a.ID, b.ID)
	}
	last := res.Steps[1]
	if len(last.Tracked) != 1 || last.Tracked[0] != "uid" {
		t.Errorf("tracked at step 2 = %v, want [uid]", last.Tracked)
	}
	if last.Via != store.EdgeCall {
		t.Errorf("via = %q, want %q", last.Via, store.EdgeCall)
	}
	if last.Confidence <= 0 {
		t.Errorf("confidence = %v, want > 0", last.Confidence)
	}
	if len(res.Alternatives) != 0 {
		t.Errorf("alternatives = %d, want 0", len(res.Alternatives))
	}
}

func TestTraceBranchingProducesAlternatives(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	a := storeFunc(t, st, "r1", "A", "(userID string)")
	b := storeFunc(t, st, "r1", "B", "(uid string)")
	c := storeFunc(t, st, "r1", "C", "(uid2 string)")
	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: a.ID, DstID: b.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "B", Meta: callMeta(t, "userID"),
	})
	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: a.ID, DstID: c.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "C", Meta: callMeta(t, "userID"),
	})

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "A", Param: "userID"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chains != 2 {
		t.Errorf("Chains = %d, want 2", res.Chains)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("best chain length = %d, want 2", len(res.Steps))
	}
	if len(res.Alternatives) != 1 || len(res.Alternatives[0]) != 2 {
		t.Fatalf("alternatives = %+v, want one chain of 2 steps", res.Alternatives)
	}
	ends := map[string]bool{
		res.Steps[1].Unit.ID:           true,
		res.Alternatives[0][1].Unit.ID: true,
	}
	if !ends[b.ID] || !ends[c.ID] {
		t.Errorf("chain endpoints = %v, want both %s and %s", ends, b.ID, c.ID)
	}
}

// TestTraceDiamondFindsBothPaths guards the DFS fix: with a global visited set
// the second path through the shared node D was truncated to a prefix chain.
func TestTraceDiamondFindsBothPaths(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	a := storeFunc(t, st, "r1", "A", "(userID string)")
	b := storeFunc(t, st, "r1", "B", "(uid string)")
	c := storeFunc(t, st, "r1", "C", "(uid2 string)")
	d := storeFunc(t, st, "r1", "D", "(xid string)")
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: a.ID, DstID: b.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "B", Meta: callMeta(t, "userID")})
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: a.ID, DstID: c.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "C", Meta: callMeta(t, "userID")})
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: b.ID, DstID: d.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "D", Meta: callMeta(t, "uid")})
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: c.ID, DstID: d.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "D", Meta: callMeta(t, "uid2")})

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "A", Param: "userID"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chains != 2 {
		t.Errorf("Chains = %d, want 2 (both diamond arms)", res.Chains)
	}
	chains := append([][]*TraceStep{res.Steps}, res.Alternatives...)
	if len(chains) != 2 {
		t.Fatalf("chains returned = %d, want 2", len(chains))
	}
	for _, chain := range chains {
		if len(chain) != 3 {
			t.Errorf("chain length = %d, want 3 (A -> mid -> D): %s", len(chain), chainIDs(chain))
			continue
		}
		if chain[2].Unit.ID != d.ID {
			t.Errorf("chain does not end at D: %s", chainIDs(chain))
		}
	}
}

func TestTraceSelfCycleTerminates(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	a := storeFunc(t, st, "r1", "A", "(userID string)")
	b := storeFunc(t, st, "r1", "B", "(userID string)")
	// A -> B -> A: cycle with identical tracked identifiers.
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: a.ID, DstID: b.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "B", Meta: callMeta(t, "userID")})
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: b.ID, DstID: a.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "A", Meta: callMeta(t, "userID")})

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "A", Param: "userID"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chains != 1 {
		t.Errorf("Chains = %d, want 1 (cycle cut once)", res.Chains)
	}
	// A -> B -> A, then the cycle key stops the walk.
	if len(res.Steps) != 3 {
		t.Errorf("steps = %d, want 3: %s", len(res.Steps), chainIDs(res.Steps))
	}
}

func TestExprMatchesAliased(t *testing.T) {
	aliases := map[string]string{"x": "userID", "y": "body.OrderId"}
	tests := []struct {
		name    string
		expr    string
		tracked []string
		aliases map[string]string
		want    bool
	}{
		{"direct match still works", "userID", []string{"user_id"}, aliases, true},
		{"alias dereference", "x", []string{"user_id"}, aliases, true},
		{"alias inside expression", "g(x)", []string{"user_id"}, aliases, true},
		{"selector alias", "y", []string{"order_id"}, aliases, true},
		{"alias to unrelated ident", "x", []string{"amount"}, aliases, false},
		{"no alias for token", "z", []string{"user_id"}, aliases, false},
		{"exact token only, no substring", "xx", []string{"user_id"}, aliases, false},
		{"nil aliases behaves like exprMatches", "x", []string{"user_id"}, nil, false},
		// Updated pin: alias chains are now followed transitively (depth <= 4),
		// so this two-step chain matches. Deeper limits are pinned in
		// TestExprMatchesAliasedTransitive.
		{"transitive chain", "x", []string{"user_id"}, map[string]string{"x": "y", "y": "userID"}, true},
		{"empty expr", "", []string{"user_id"}, aliases, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exprMatchesAliased(tt.expr, tt.tracked, tt.aliases); got != tt.want {
				t.Errorf("exprMatchesAliased(%q, %v, %v) = %v, want %v",
					tt.expr, tt.tracked, tt.aliases, got, tt.want)
			}
		})
	}
}

// TestTraceFollowsAliasedCallEdge: the argument at the call site is a local
// alias ("x") of the traced parameter; without Meta.Aliases the chain would
// stop at A.
func TestTraceFollowsAliasedCallEdge(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	a := storeFunc(t, st, "r1", "A", "(userID string)")
	b := storeFunc(t, st, "r1", "B", "(uid string)")
	meta, err := json.Marshal(&store.EdgeMeta{
		Args:    []string{"x"},
		Aliases: map[string]string{"x": "userID"},
	})
	if err != nil {
		t.Fatal(err)
	}
	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: a.ID, DstID: b.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "B", Line: 7,
		Meta: string(meta),
	})

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "A", Param: "user_id"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chains != 1 {
		t.Errorf("Chains = %d, want 1", res.Chains)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2 (alias x -> userID must match): %s",
			len(res.Steps), chainIDs(res.Steps))
	}
	if res.Steps[1].Unit.ID != b.ID {
		t.Errorf("step 2 unit = %s, want %s", res.Steps[1].Unit.ID, b.ID)
	}
	if last := res.Steps[1]; len(last.Tracked) != 1 || last.Tracked[0] != "uid" {
		t.Errorf("tracked at step 2 = %v, want [uid]", last.Tracked)
	}
}

// TestTraceWithoutAliasesDoesNotMatch pins the counterfactual: the same edge
// without Aliases yields no chain, proving the alias is what makes the hop.
func TestTraceWithoutAliasesDoesNotMatch(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	a := storeFunc(t, st, "r1", "A", "(userID string)")
	b := storeFunc(t, st, "r1", "B", "(uid string)")
	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: a.ID, DstID: b.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "B",
		Meta: callMeta(t, "x"), // arg "x", no aliases
	})

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "A", Param: "user_id"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chains != 0 {
		t.Errorf("Chains = %d, want 0 (arg x must not match user_id without an alias)", res.Chains)
	}
}

// TestTraceDedupsOnTrackedIdentifiers: the same two units linked twice, each
// call putting the value in a different parameter, are two distinct findings.
func TestTraceDedupsOnTrackedIdentifiers(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	a := storeFunc(t, st, "r1", "A", "(userID string)")
	b := storeFunc(t, st, "r1", "B", "(first string, second string)")
	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: a.ID, DstID: b.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "B", Meta: callMeta(t, "userID", "other"),
	})
	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: a.ID, DstID: b.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "B", Meta: callMeta(t, "other", "userID"),
	})

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "A", Param: "userID"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chains != 2 {
		t.Errorf("Chains = %d, want 2 (same units, different parameters)", res.Chains)
	}
	tracked := map[string]bool{}
	for _, chain := range append([][]*TraceStep{res.Steps}, res.Alternatives...) {
		tracked[chain[len(chain)-1].Tracked[0]] = true
	}
	if !tracked["first"] || !tracked["second"] {
		t.Errorf("tracked endpoints = %v, want both first and second", tracked)
	}
}

// TestTraceTruncatedChainDoesNotWin: a walk cut off at the depth limit says
// nothing about where the value ends up, so it must not beat a shorter chain
// that reached a sink — and it must say that it was truncated.
func TestTraceTruncatedChainDoesNotWin(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	a := storeFunc(t, st, "r1", "A", "(userID string)")
	b := storeFunc(t, st, "r1", "B", "(uid string)")
	c := storeFunc(t, st, "r1", "C", "(uid string)")
	sink := storeFunc(t, st, "r1", "Sink", "(uid string)")
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: a.ID, DstID: b.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "B", Meta: callMeta(t, "userID")})
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: b.ID, DstID: c.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "C", Meta: callMeta(t, "uid")})
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: a.ID, DstID: sink.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "Sink", Meta: callMeta(t, "userID")})

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "A", Param: "userID", MaxDepth: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 2 || res.Steps[1].Unit.ID != sink.ID {
		t.Fatalf("best chain = %s, want the complete A->Sink chain", chainIDs(res.Steps))
	}
	if len(res.Alternatives) != 1 {
		t.Fatalf("alternatives = %d, want the truncated chain", len(res.Alternatives))
	}
	alt := res.Alternatives[0]
	if note := alt[len(alt)-1].Note; !strings.Contains(note, "truncated") {
		t.Errorf("truncated chain last note = %q, want it marked as truncated", note)
	}
}

// TestFinishKeepsBestChainsAtCap: once maxCompletedChains walks are recorded,
// a better one must displace the worst instead of being dropped because it was
// discovered later.
func TestFinishKeepsBestChainsAtCap(t *testing.T) {
	tr := &tracer{seen: map[string]bool{}}
	for i := 0; i < maxCompletedChains; i++ {
		tr.finish([]*TraceStep{step("start", ""), step("local"+strconv.Itoa(i), store.EdgeCall)}, "")
	}
	if len(tr.completed) != maxCompletedChains {
		t.Fatalf("completed = %d, want %d", len(tr.completed), maxCompletedChains)
	}

	best := []*TraceStep{step("start", ""), step("remote", store.EdgeRPCCall)}
	tr.finish(best, "")
	if tr.discovered != maxCompletedChains+1 {
		t.Errorf("discovered = %d, want every distinct chain counted", tr.discovered)
	}
	found := false
	for _, c := range tr.completed {
		if c.steps[1].Unit.ID == "remote" {
			found = true
		}
	}
	if !found {
		t.Error("a chain crossing a service boundary must displace a purely local one at the cap")
	}

	// A worse chain at the cap is dropped.
	tr.finish([]*TraceStep{step("start", ""), step("extra", store.EdgeCall)}, "")
	for _, c := range tr.completed {
		if c.steps[1].Unit.ID == "extra" {
			t.Error("a chain no better than the worst kept one must be dropped")
		}
	}
}

// countingStore records how often the tracer reaches storage, so that the
// per-hop query storm the batching loader removes cannot come back.
type countingStore struct {
	store.Storage
	unitByID int
	services int
}

func (c *countingStore) GetASTUnitByID(ctx context.Context, id string) (*domain.ASTUnit, error) {
	c.unitByID++
	return c.Storage.GetASTUnitByID(ctx, id)
}

func (c *countingStore) GetASTUnits(ctx context.Context, opts domain.QueryOpts) ([]*domain.ASTUnit, error) {
	if opts.Kind == store.KindService {
		c.services++
	}
	return c.Storage.GetASTUnits(ctx, opts)
}

func TestTraceUsesBatchingLoader(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	a := storeFunc(t, st, "r1", "A", "(userID string)")
	b := storeFunc(t, st, "r1", "B", "(uid string)")
	c := storeFunc(t, st, "r1", "C", "(uid string)")
	d := storeFunc(t, st, "r1", "D", "(uid string)")
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: a.ID, DstID: b.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "B", Meta: callMeta(t, "userID")})
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: a.ID, DstID: c.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "C", Meta: callMeta(t, "userID")})
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: b.ID, DstID: d.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "D", Meta: callMeta(t, "uid")})
	storeEdge(t, st, &domain.Edge{RepoID: "r1", SrcID: c.ID, DstID: d.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "D", Meta: callMeta(t, "uid")})

	cs := &countingStore{Storage: st}
	res, err := New(cs).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "A", Param: "userID"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chains != 2 {
		t.Fatalf("Chains = %d, want 2", res.Chains)
	}
	if cs.services != 1 {
		t.Errorf("service lookups = %d, want 1 for a single-repo trace", cs.services)
	}
	if cs.unitByID != 0 {
		t.Errorf("single-unit lookups = %d, want 0 (units arrive batched)", cs.unitByID)
	}
}

// storeTable stores a db_table unit for the "db:<name>" contract key.
func storeTable(t *testing.T, st *sqlite.SQLite, repoID, name string) *domain.ASTUnit {
	t.Helper()
	return storeUnit(t, st, &domain.ASTUnit{
		RepoID: repoID, FilePath: "migrations/001_" + name + ".sql", Language: "sql",
		Kind: store.KindDBTable, Name: name, Qualified: "db:" + name,
	})
}

func writeMeta(t *testing.T, column, expr string, args ...string) string {
	t.Helper()
	b, err := json.Marshal(&store.EdgeMeta{
		Args:   args,
		Fields: map[string]string{column: expr},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTraceContinuesThroughTable: a value written into a table must reach the
// units reading that table, including across repositories — the writer and the
// reader are joined by the db: key alone.
func TestTraceContinuesThroughTable(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	writer := storeFunc(t, st, "r1", "CreateOrder", "(userID string)")
	table := storeTable(t, st, "r2", "orders")
	reader := storeFunc(t, st, "r2", "LoadOrder", "(id string)")
	sink := storeFunc(t, st, "r2", "Notify", "(uid string)")

	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: writer.ID, DstID: table.ID, DstRepoID: "r2",
		Kind: store.EdgeWritesTo, DstName: "db:orders", Line: 20,
		Meta: writeMeta(t, "user_id", "userID", "INSERT INTO orders (user_id) VALUES ($1)", "userID"),
	})
	storeEdge(t, st, &domain.Edge{
		RepoID: "r2", SrcID: reader.ID, DstID: table.ID, DstRepoID: "r2",
		Kind: store.EdgeReadsFrom, DstName: "db:orders", Line: 7,
	})
	storeEdge(t, st, &domain.Edge{
		RepoID: "r2", SrcID: reader.ID, DstID: sink.ID, DstRepoID: "r2",
		Kind: store.EdgeCall, DstName: "Notify", Meta: callMeta(t, "row.UserID"),
	})

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "CreateOrder", Param: "userID"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 4 {
		t.Fatalf("steps = %d, want 4 (writer -> table -> reader -> sink): %s",
			len(res.Steps), chainIDs(res.Steps))
	}
	want := []string{writer.ID, table.ID, reader.ID, sink.ID}
	for i, id := range want {
		if res.Steps[i].Unit.ID != id {
			t.Fatalf("chain = %s, want writer -> table -> reader -> sink", chainIDs(res.Steps))
		}
	}
	read := res.Steps[2]
	if read.Via != store.EdgeReadsFrom {
		t.Errorf("via at the reader = %q, want %q", read.Via, store.EdgeReadsFrom)
	}
	if len(read.Tracked) != 1 || read.Tracked[0] != "user_id" {
		t.Errorf("tracked at the reader = %v, want [user_id] (the written column)", read.Tracked)
	}
	if !strings.Contains(read.Note, "orders") {
		t.Errorf("note at the reader = %q, want it to name the table", read.Note)
	}
	if read.Confidence >= res.Steps[1].Confidence {
		t.Errorf("confidence across the table = %v, want it below the write step's %v",
			read.Confidence, res.Steps[1].Confidence)
	}
}

// TestTraceThroughLinkedTableAcrossRepos: the writer, the table declaration and
// the reader live in three repositories and are joined only by the "db:" key,
// so the hop exists only if the linker resolved both sides of it.
func TestTraceThroughLinkedTableAcrossRepos(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	writer := storeFunc(t, st, "repoA", "CreateOrder", "(userID string)")
	table := storeTable(t, st, "repoB", "orders")
	reader := storeFunc(t, st, "repoC", "LoadOrder", "(id string)")
	storeEdge(t, st, &domain.Edge{
		RepoID: "repoA", SrcID: writer.ID,
		Kind: store.EdgeWritesTo, DstName: "db:orders",
		Meta: writeMeta(t, "user_id", "userID"),
	})
	readEdge := storeEdge(t, st, &domain.Edge{
		RepoID: "repoC", SrcID: reader.ID,
		Kind: store.EdgeReadsFrom, DstName: "db:orders",
	})

	if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}
	got := edgeByID(t, st, store.EdgeReadsFrom, readEdge.ID)
	if got.DstID != table.ID || got.DstRepoID != "repoB" {
		t.Fatalf("reads_from resolved to %s@%s, want the table %s@repoB",
			got.DstID, got.DstRepoID, table.ID)
	}

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "repoA", Symbol: "CreateOrder", Param: "userID"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 3 || res.Steps[2].Unit.ID != reader.ID {
		t.Fatalf("chain = %s, want it to reach the reader in repoC", chainIDs(res.Steps))
	}
}

// TestTraceTableHopKeepsWrittenColumn: the reader is followed structurally, but
// the tracked set stays the written column, so a value the reader does not
// carry on does not produce a hop out of it.
func TestTraceTableHopKeepsWrittenColumn(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	writer := storeFunc(t, st, "r1", "CreateOrder", "(userID string)")
	table := storeTable(t, st, "r1", "orders")
	reader := storeFunc(t, st, "r1", "LoadTotals", "(id string)")
	other := storeFunc(t, st, "r1", "Report", "(amount int)")

	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: writer.ID, DstID: table.ID, DstRepoID: "r1",
		Kind: store.EdgeWritesTo, DstName: "db:orders",
		Meta: writeMeta(t, "user_id", "userID"),
	})
	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: reader.ID, DstID: table.ID, DstRepoID: "r1",
		Kind: store.EdgeReadsFrom, DstName: "db:orders",
	})
	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: reader.ID, DstID: other.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "Report", Meta: callMeta(t, "row.Amount"),
	})

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "CreateOrder", Param: "userID"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 3 || res.Steps[2].Unit.ID != reader.ID {
		t.Fatalf("chain = %s, want it to stop at the reader", chainIDs(res.Steps))
	}
}

// TestTraceTableReaderHopsAreOnePerUnit: a unit querying the same table
// several times is one branch. Without the dedup the DFS walks its whole
// subtree once per statement, spending the expansion budget on chains the
// recorder then throws away as duplicates.
func TestTraceTableReaderHopsAreOnePerUnit(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	table := storeTable(t, st, "r1", "orders")
	reader := storeFunc(t, st, "r1", "LoadOrder", "(id string)")
	for j := 0; j < 3; j++ {
		storeEdge(t, st, &domain.Edge{
			RepoID: "r1", SrcID: reader.ID, DstID: table.ID, DstRepoID: "r1",
			Kind: store.EdgeReadsFrom, DstName: "db:orders", Line: j + 1,
		})
	}

	g := New(st)
	tr := &tracer{g: g, ctx: ctx, ld: newLoader(g), edges: map[string][]*domain.Edge{}}
	hops := tr.expand(table, []string{"user_id"})
	if len(hops) != 1 {
		t.Fatalf("hops out of the table = %d, want 1 per reading unit", len(hops))
	}
	if hops[0].unit.ID != reader.ID {
		t.Errorf("hop target = %s, want the reader %s", hops[0].unit.ID, reader.ID)
	}
	if hops[0].factor >= 1.0 {
		t.Errorf("hop factor = %v, want a database hop to be weaker than a direct call", hops[0].factor)
	}
}

// TestTraceTableWithManyReadersStaysBounded: a hot table fans out to every unit
// reading it, and each reader writes the value back. The walk must terminate on
// the existing budgets, count one hop per reader however many statements it
// runs, and still return at most maxAlternatives chains.
func TestTraceTableWithManyReadersStaysBounded(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	const readers = 80
	writer := storeFunc(t, st, "r1", "CreateOrder", "(userID string)")
	table := storeTable(t, st, "r1", "orders")
	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: writer.ID, DstID: table.ID, DstRepoID: "r1",
		Kind: store.EdgeWritesTo, DstName: "db:orders",
		Meta: writeMeta(t, "user_id", "userID"),
	})
	for i := 0; i < readers; i++ {
		r := storeFunc(t, st, "r1", "Reader"+strconv.Itoa(i), "(id string)")
		reads := 1
		if i == 0 {
			reads = 3 // one unit, several SELECTs on the same table
		}
		for j := 0; j < reads; j++ {
			storeEdge(t, st, &domain.Edge{
				RepoID: "r1", SrcID: r.ID, DstID: table.ID, DstRepoID: "r1",
				Kind: store.EdgeReadsFrom, DstName: "db:orders", Line: j + 1,
			})
		}
		// Every reader writes the value back: the cycle must be cut at the
		// table rather than re-expanded per reader.
		storeEdge(t, st, &domain.Edge{
			RepoID: "r1", SrcID: r.ID, DstID: table.ID, DstRepoID: "r1",
			Kind: store.EdgeWritesTo, DstName: "db:orders",
			Meta: writeMeta(t, "user_id", "userID"),
		})
	}

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "CreateOrder", Param: "userID"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chains != readers {
		t.Errorf("chains = %d, want %d (one per reader, duplicate statements deduped)",
			res.Chains, readers)
	}
	if len(res.Alternatives) > maxAlternatives {
		t.Errorf("alternatives = %d, want at most %d", len(res.Alternatives), maxAlternatives)
	}
	for _, chain := range append([][]*TraceStep{res.Steps}, res.Alternatives...) {
		if len(chain) > 4 {
			t.Errorf("chain = %s, want the write-back cycle cut at the table", chainIDs(chain))
		}
	}
}

func chainIDs(chain []*TraceStep) string {
	s := ""
	for i, st := range chain {
		if i > 0 {
			s += "->"
		}
		s += st.Unit.Name
	}
	return s
}

// --- transitive alias chains (additions) ---

func TestExprMatchesAliasedTransitive(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		tracked []string
		aliases map[string]string
		want    bool
	}{
		{"depth 2 chain", "x", []string{"user_id"},
			map[string]string{"x": "y", "y": "userID"}, true},
		{"depth 3 chain", "x", []string{"user_id"},
			map[string]string{"x": "y", "y": "z", "z": "userID"}, true},
		{"depth 3 chain inside expression", "g(x)", []string{"user_id"},
			map[string]string{"x": "y", "y": "z", "z": "userID"}, true},
		{"depth 4 chain at the limit", "a", []string{"user_id"},
			map[string]string{"a": "b", "b": "c", "c": "d", "d": "userID"}, true},
		// Pin: chains needing more than maxAliasDepth dereferences do not match.
		{"depth 5 chain beyond the limit", "a", []string{"user_id"},
			map[string]string{"a": "b", "b": "c", "c": "d", "d": "e", "e": "userID"}, false},
		// Cycles terminate and do not match.
		{"two-node cycle", "x", []string{"user_id"},
			map[string]string{"x": "y", "y": "x"}, false},
		{"self cycle", "x", []string{"user_id"},
			map[string]string{"x": "x"}, false},
		// A cycle on one branch must not block a match on another.
		{"cycle plus matching branch", "g(x, w)", []string{"user_id"},
			map[string]string{"x": "y", "y": "x", "w": "userID"}, true},
		// Call-text alias: x = extractUserID(req) matches via token "userID"
		// inside the recorded call expression.
		{"call-result alias", "x", []string{"user_id"},
			map[string]string{"x": "extractUserID(req)"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exprMatchesAliased(tt.expr, tt.tracked, tt.aliases); got != tt.want {
				t.Errorf("exprMatchesAliased(%q, %v, %v) = %v, want %v",
					tt.expr, tt.tracked, tt.aliases, got, tt.want)
			}
		})
	}
}

// TestTraceFollowsTransitiveAliasChain: the call-site argument "x" reaches the
// traced parameter only through a two-step alias chain in the edge meta.
func TestTraceFollowsTransitiveAliasChain(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	a := storeFunc(t, st, "r1", "A", "(userID string)")
	b := storeFunc(t, st, "r1", "B", "(uid string)")
	meta, err := json.Marshal(&store.EdgeMeta{
		Args:    []string{"x"},
		Aliases: map[string]string{"x": "y", "y": "userID"},
	})
	if err != nil {
		t.Fatal(err)
	}
	storeEdge(t, st, &domain.Edge{
		RepoID: "r1", SrcID: a.ID, DstID: b.ID, DstRepoID: "r1",
		Kind: store.EdgeCall, DstName: "B", Line: 5,
		Meta: string(meta),
	})

	res, err := New(st).Trace(ctx, &TraceRequest{RepoID: "r1", Symbol: "A", Param: "user_id"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chains != 1 {
		t.Errorf("Chains = %d, want 1", res.Chains)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2 (chain x -> y -> userID must match): %s",
			len(res.Steps), chainIDs(res.Steps))
	}
	if res.Steps[1].Unit.ID != b.ID {
		t.Errorf("step 2 unit = %s, want %s", res.Steps[1].Unit.ID, b.ID)
	}
	if last := res.Steps[1]; len(last.Tracked) != 1 || last.Tracked[0] != "uid" {
		t.Errorf("tracked at step 2 = %v, want [uid]", last.Tracked)
	}
}
