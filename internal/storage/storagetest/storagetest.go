// Package storagetest is the conformance suite for storage.Storage.
//
// Two backends implement that interface over different drivers, and until this
// suite existed each was tested by its own hand-ported file. They drifted:
// UpdateRepoStatus on a missing repo reported ErrNotFound on one backend and
// success on the other, which is exactly the kind of difference no unit test of
// a single backend can see. Behaviour the service layer relies on is asserted
// here once, against whichever store the caller opens.
//
// The suite runs against a shared database: every subtest works under its own
// repository id and removes its rows afterwards, and nothing asserts on global
// state (job claiming takes the oldest pending job of any repo, so a shared
// instance cannot say which job that is).
package storagetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// Run executes the conformance suite. open must return a store that is ready to
// use and registers its own teardown.
func Run(t *testing.T, open func(t *testing.T) storage.Storage) {
	t.Helper()
	t.Run("RepoLifecycle", func(t *testing.T) { testRepoLifecycle(t, open(t)) })
	t.Run("RepoClaim", func(t *testing.T) { testRepoClaim(t, open(t)) })
	t.Run("RepoActive", func(t *testing.T) { testRepoActive(t, open(t)) })
	t.Run("Files", func(t *testing.T) { testFiles(t, open(t)) })
	t.Run("UnitQueries", func(t *testing.T) { testUnitQueries(t, open(t)) })
	t.Run("UnitRepoScope", func(t *testing.T) { testUnitRepoScope(t, open(t)) })
	t.Run("SymbolLookup", func(t *testing.T) { testSymbolLookup(t, open(t)) })
	t.Run("InsertionOrder", func(t *testing.T) { testInsertionOrder(t, open(t)) })
	t.Run("UnitsAndEdges", func(t *testing.T) { testUnitsAndEdges(t, open(t)) })
	t.Run("EdgeResolution", func(t *testing.T) { testEdgeResolution(t, open(t)) })
	t.Run("Coverage", func(t *testing.T) { testCoverage(t, open(t)) })
	t.Run("Jobs", func(t *testing.T) { testJobs(t, open(t)) })
}

// newRepoID returns an id no other test run is using, so the suite is safe on a
// shared database.
func newRepoID(prefix string) string {
	return fmt.Sprintf("storagetest-%s-%d", prefix, time.Now().UnixNano())
}

// seedRepo stores a repository and removes everything under it afterwards.
func seedRepo(t *testing.T, st storage.Storage, repoID string) *repos.Repo {
	t.Helper()
	ctx := context.Background()
	r := &repos.Repo{
		ID:        repoID,
		Name:      "conformance",
		Source:    repos.SourceTypeLocal,
		Path:      "/tmp/" + repoID,
		Status:    repos.StatusIdle,
		CreatedAt: 1,
	}
	if err := st.StoreRepo(ctx, r); err != nil {
		t.Fatalf("StoreRepo: %v", err)
	}
	t.Cleanup(func() {
		_ = st.DeleteEdgesByRepo(ctx, repoID)
		_ = st.DeleteASTUnitsByRepo(ctx, repoID)
		_ = st.DeleteFilesByRepo(ctx, repoID)
		_ = st.DeleteRepoCoverage(ctx, repoID)
		_ = st.DeleteRepo(ctx, repoID)
	})
	return r
}

func testRepoLifecycle(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	repoID := newRepoID("repo")
	seedRepo(t, st, repoID)

	got, err := st.GetRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if got.Name != "conformance" || got.Status != repos.StatusIdle {
		t.Errorf("GetRepo = %+v, want name=conformance status=idle", got)
	}

	// A status write reaches the row and clears the claim fields.
	if err := st.UpdateRepoStatus(ctx, repoID, repos.StatusError, "boom", 42); err != nil {
		t.Fatalf("UpdateRepoStatus: %v", err)
	}
	got, err = st.GetRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("GetRepo after status update: %v", err)
	}
	if got.Status != repos.StatusError || got.LastError != "boom" || got.IndexedAt != 42 {
		t.Errorf("after UpdateRepoStatus = %+v, want status=error last_error=boom indexed_at=42", got)
	}

	// The conformance point the two backends used to disagree on: a status
	// write for a repo that is gone is an error, not a silent success. The repo
	// was deleted under a running pass, and the caller's log line is the only
	// trace left of it.
	if err := st.UpdateRepoStatus(ctx, repoID+"-missing", repos.StatusIdle, "", 0); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("UpdateRepoStatus on a missing repo = %v, want storage.ErrNotFound", err)
	}

	// Re-registering a repo updates its definition without resetting lifecycle
	// state owned by the indexing pipeline.
	if err := st.StoreRepo(ctx, &repos.Repo{
		ID: repoID, Name: "renamed", Source: repos.SourceTypeLocal,
		Path: "/tmp/other", Status: repos.StatusIdle,
	}); err != nil {
		t.Fatalf("StoreRepo (re-register): %v", err)
	}
	got, err = st.GetRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("GetRepo after re-register: %v", err)
	}
	if got.Name != "renamed" {
		t.Errorf("name after re-register = %q, want renamed", got.Name)
	}
	if got.Status != repos.StatusError || got.LastError != "boom" || got.IndexedAt != 42 {
		t.Errorf("re-registering reset lifecycle state: %+v", got)
	}

	// ListRepos returns the stored repositories. The suite may share a database
	// with other tests, so it asserts that this repo is listed rather than that
	// it is the only one.
	list, err := st.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	var listed bool
	for _, r := range list {
		if r.ID == repoID {
			listed = true
		}
	}
	if !listed {
		t.Errorf("ListRepos returned %d repos, none of them %s", len(list), repoID)
	}

	if err := st.DeleteRepo(ctx, repoID); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	if _, err := st.GetRepo(ctx, repoID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("GetRepo after delete = %v, want storage.ErrNotFound", err)
	}
}

func testRepoClaim(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	repoID := newRepoID("claim")
	seedRepo(t, st, repoID)

	claimed, err := st.ClaimRepoForIndexing(ctx, repoID, "owner-a", 300)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v; want true, nil", claimed, err)
	}
	got, err := st.GetRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if got.Status != repos.StatusIndexing {
		t.Errorf("status after claim = %s, want indexing", got.Status)
	}

	// A live claim is exclusive: the second worker is told no rather than
	// being allowed to index the same repository concurrently.
	claimed, err = st.ClaimRepoForIndexing(ctx, repoID, "owner-b", 300)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Error("second claim succeeded while the first was live")
	}

	if _, err := st.ClaimRepoForIndexing(ctx, repoID+"-missing", "owner-a", 300); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("claim on a missing repo = %v, want storage.ErrNotFound", err)
	}

	// Writing a terminal status releases the claim, so the next pass may take
	// it. (A claim that expires without anyone writing that status is taken
	// over on time, which needs a clock the suite cannot move portably; the
	// SQLite tests cover it.)
	if err := st.UpdateRepoStatus(ctx, repoID, repos.StatusIdle, "", 1); err != nil {
		t.Fatalf("UpdateRepoStatus: %v", err)
	}
	if claimed, err = st.ClaimRepoForIndexing(ctx, repoID, "owner-b", 300); err != nil || !claimed {
		t.Errorf("claim after release = %v, %v; want true, nil", claimed, err)
	}

	// A repo left claimed by a process that died is recovered at startup.
	// ResetStuckRepos is repository-wide by nature, so this asserts only that
	// its own repo came back to idle with the claim cleared — the count covers
	// whatever else the database happened to hold.
	if _, err := st.ClaimRepoForIndexing(ctx, repoID, "dead-owner", 3600); err != nil {
		t.Fatalf("claim before reset: %v", err)
	}
	if _, err := st.ResetStuckRepos(ctx, true); err != nil {
		t.Fatalf("ResetStuckRepos: %v", err)
	}
	got, _ = st.GetRepo(ctx, repoID)
	if got.Status != repos.StatusIdle {
		t.Errorf("status after forced reset = %s, want idle", got.Status)
	}
	if claimed, err := st.ClaimRepoForIndexing(ctx, repoID, "next-owner", 300); err != nil || !claimed {
		t.Errorf("claim after reset = %v, %v; want the claim to have been released", claimed, err)
	}
	if err := st.UpdateRepoStatus(ctx, repoID, repos.StatusIdle, "", 1); err != nil {
		t.Fatalf("release after reset: %v", err)
	}

	// The commit cursor and the in-flight SHA are separate fields: /sync-state
	// tells "in flight" from "lost" by reading them apart.
	if err := st.UpdateRepoLastCommit(ctx, repoID, "sha-applied"); err != nil {
		t.Fatalf("UpdateRepoLastCommit: %v", err)
	}
	if err := st.SetRepoPendingCommit(ctx, repoID, "sha-running"); err != nil {
		t.Fatalf("SetRepoPendingCommit: %v", err)
	}
	got, _ = st.GetRepo(ctx, repoID)
	if got.LastCommit != "sha-applied" || got.PendingCommit != "sha-running" {
		t.Errorf("commit fields = %q/%q, want sha-applied/sha-running", got.LastCommit, got.PendingCommit)
	}
	if err := st.SetRepoPendingCommit(ctx, repoID, ""); err != nil {
		t.Fatalf("SetRepoPendingCommit(clear): %v", err)
	}
	if got, _ = st.GetRepo(ctx, repoID); got.PendingCommit != "" {
		t.Errorf("pending commit after clear = %q, want empty", got.PendingCommit)
	}
}

// testRepoActive covers the working set: which repositories a run is about.
// The rules it pins are the ones the service layer reads — a registration never
// changes membership, a switch names the whole set rather than adding to it,
// and an inactive repository is out of the way rather than gone.
//
// SetActiveRepos is repository-wide by nature (it clears every row it does not
// name), so like ResetStuckRepos this asserts only about its own repositories
// on a database the suite may share.
func testRepoActive(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	base := newRepoID("active")
	a, b, c := base+"-a", base+"-b", base+"-c"
	for _, id := range []string{a, b, c} {
		seedRepo(t, st, id)
	}

	isActive := func(id string) bool {
		t.Helper()
		got, err := st.GetRepo(ctx, id)
		if err != nil {
			t.Fatalf("GetRepo(%s): %v", id, err)
		}
		return got.Active
	}
	inActiveList := func(id string) bool {
		t.Helper()
		list, err := st.ListActiveRepos(ctx)
		if err != nil {
			t.Fatalf("ListActiveRepos: %v", err)
		}
		for _, r := range list {
			if r.ID == id {
				if !r.Active {
					t.Errorf("ListActiveRepos returned %s with active=false", id)
				}
				return true
			}
		}
		return false
	}
	set := func(ids ...string) {
		t.Helper()
		if err := st.SetActiveRepos(ctx, ids); err != nil {
			t.Fatalf("SetActiveRepos(%v): %v", ids, err)
		}
	}

	// A newly registered repository is active. Nothing in the system asks to be
	// made visible, so anything else would hide every repository added through
	// the API from the searches that do not name it.
	for _, id := range []string{a, b, c} {
		if !isActive(id) || !inActiveList(id) {
			t.Fatalf("%s is not active on registration", id)
		}
	}

	// State the switch must not disturb: b has been indexed and has failed once.
	if err := st.UpdateRepoStatus(ctx, b, repos.StatusError, "boom", 99); err != nil {
		t.Fatalf("UpdateRepoStatus: %v", err)
	}

	set(a)
	if !isActive(a) || isActive(b) || isActive(c) {
		t.Errorf("after SetActiveRepos(a): active = %v/%v/%v, want only a", isActive(a), isActive(b), isActive(c))
	}
	if !inActiveList(a) || inActiveList(b) || inActiveList(c) {
		t.Error("ListActiveRepos disagrees with the stored flags")
	}

	// Re-registering an inactive repository leaves it inactive. Every startup
	// re-registers whatever its source finds, so a registration that raised the
	// flag would quietly redefine the working set.
	if err := st.StoreRepo(ctx, &repos.Repo{
		ID: b, Name: "conformance", Source: repos.SourceTypeLocal,
		Path: "/tmp/" + b, Status: repos.StatusIdle, Active: true,
	}); err != nil {
		t.Fatalf("StoreRepo (re-register): %v", err)
	}
	if isActive(b) {
		t.Error("re-registering an inactive repository reactivated it")
	}

	// A switch names the whole set: the previous member drops out, and an id
	// that names no repository is ignored rather than rejected.
	set(b, c, base+"-missing")
	if isActive(a) || !isActive(b) || !isActive(c) {
		t.Errorf("after SetActiveRepos(b, c): active = %v/%v/%v, want b and c", isActive(a), isActive(b), isActive(c))
	}

	// Inactive is a view, not a lifecycle state: b came back with the status,
	// error and timestamp it had before it was put away.
	got, err := st.GetRepo(ctx, b)
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if got.Status != repos.StatusError || got.LastError != "boom" || got.IndexedAt != 99 {
		t.Errorf("the switch disturbed lifecycle state: %+v", got)
	}

	// An empty set is a valid request, and it leaves nothing active.
	set()
	if isActive(a) || isActive(b) || isActive(c) {
		t.Error("SetActiveRepos with no ids left a repository active")
	}

	// And a later run pointed back at them brings them all back.
	set(a, b, c)
	if !isActive(a) || !isActive(b) || !isActive(c) {
		t.Error("repositories did not come back when they were named again")
	}
}

func testUnitQueries(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	repoID := newRepoID("units-query")
	seedRepo(t, st, repoID)

	units := []*storage.ASTUnit{
		{RepoID: repoID, FilePath: "a.go", Language: "go", Kind: "function",
			Name: "First", Qualified: "pkg.First", StartLine: 10, EndLine: 20,
			Meta: storage.EncodeUnitMeta(&storage.UnitMeta{Root: "svc", Summary: "does the first thing"})},
		{RepoID: repoID, FilePath: "a.go", Language: "go", Kind: "function",
			Name: "Second", Qualified: "pkg.Second", StartLine: 30, EndLine: 40},
		{RepoID: repoID, FilePath: "b.go", Language: "go", Kind: "type",
			Name: "Thing", Qualified: "pkg.Thing", StartLine: 1, EndLine: 5},
	}
	if err := st.BatchStoreASTUnits(ctx, units); err != nil {
		t.Fatalf("BatchStoreASTUnits: %v", err)
	}

	// Limit bounds the result rather than being ignored.
	got, err := st.GetASTUnits(ctx, storage.QueryOpts{RepoID: repoID, Limit: 2})
	if err != nil || len(got) != 2 {
		t.Errorf("GetASTUnits(limit=2) = %d units, %v; want 2, nil", len(got), err)
	}

	byIDs, err := st.GetASTUnitsByIDs(ctx, []string{units[0].ID, units[2].ID})
	if err != nil || len(byIDs) != 2 {
		t.Fatalf("GetASTUnitsByIDs = %d units, %v; want 2, nil", len(byIDs), err)
	}

	// Line containment is evaluated by the store: a file can hold more units
	// than any client-side page, so filtering after a limit would lose symbols
	// late in large files.
	inRange, err := st.GetASTUnits(ctx, storage.QueryOpts{RepoID: repoID, FilePath: "a.go", Line: 15})
	if err != nil || len(inRange) != 1 || inRange[0].Name != "First" {
		t.Errorf("GetASTUnits(line=15) = %+v, %v; want only First", inRange, err)
	}
	if outside, _ := st.GetASTUnits(ctx, storage.QueryOpts{RepoID: repoID, FilePath: "a.go", Line: 25}); len(outside) != 0 {
		t.Errorf("GetASTUnits(line=25) = %d units, want none (the line is between symbols)", len(outside))
	}

	// Unit meta survives the round trip: the summary pass writes it and the
	// card builder reads it back.
	stored, err := st.GetASTUnitByID(ctx, units[0].ID)
	if err != nil {
		t.Fatalf("GetASTUnitByID: %v", err)
	}
	if m := storage.DecodeUnitMeta(stored.Meta); m.Root != "svc" || m.Summary != "does the first thing" {
		t.Errorf("unit meta round trip = %+v, want root=svc and the summary", m)
	}

	// Deleting by a set of paths clears exactly those files, not their
	// neighbours in the same repository.
	if err := st.DeleteASTUnitsByFiles(ctx, repoID, []string{"a.go"}); err != nil {
		t.Fatalf("DeleteASTUnitsByFiles: %v", err)
	}
	left, err := st.GetASTUnits(ctx, storage.QueryOpts{RepoID: repoID})
	if err != nil || len(left) != 1 || left[0].FilePath != "b.go" {
		t.Errorf("units after per-file delete = %+v, %v; want only b.go", left, err)
	}
}

// testUnitRepoScope covers the filter retrieval scoping is built on: a *set* of
// repositories rather than one. A run is about the repositories its source
// named, which is a list, and RepoID — one string compiled to repo_id = ? —
// cannot say it. Both backends compile the set through sqlutil, so the shape of
// the SQL is asserted there; what belongs here is what the engines return.
func testUnitRepoScope(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	here, there := newRepoID("scope-here"), newRepoID("scope-there")
	seedRepo(t, st, here)
	seedRepo(t, st, there)

	// The suite shares a database, so the fixture carries a name no other
	// subtest holds: that is what makes an *unscoped* query readable here, since
	// otherwise it would answer with every unit the database has.
	name := fmt.Sprintf("ScopedSymbol%d", time.Now().UnixNano())
	unit := func(repoID, path, unitName string) *storage.ASTUnit {
		return &storage.ASTUnit{
			RepoID: repoID, FilePath: path, Language: "go", Kind: "function",
			Name: unitName, Qualified: "pkg." + unitName, StartLine: 1, EndLine: 2,
		}
	}
	if err := st.BatchStoreASTUnits(ctx, []*storage.ASTUnit{
		unit(here, "a.go", name),
		unit(there, "b.go", name),
		// A substring-only match, reachable by the fallback tier alone. It is in
		// the far repository so that a scope that stops at the first statement
		// can be told apart from one that covers both.
		unit(there, "helper.go", name+"Helper"),
	}); err != nil {
		t.Fatalf("BatchStoreASTUnits: %v", err)
	}

	repoIDs := func(got []*storage.ASTUnit) []string {
		out := make([]string, 0, len(got))
		for _, u := range got {
			out = append(out, u.RepoID)
		}
		return out
	}
	query := func(t *testing.T, opts storage.QueryOpts) []*storage.ASTUnit {
		t.Helper()
		got, err := st.GetASTUnits(ctx, opts)
		if err != nil {
			t.Fatalf("GetASTUnits(%+v): %v", opts, err)
		}
		return got
	}

	// No set is every repository, which is the property the whole scheme rests
	// on: a scope covering everything is expressed by leaving the filter off,
	// never by listing every repository.
	if got := query(t, storage.QueryOpts{Name: name}); len(got) != 2 {
		t.Errorf("unscoped query = %v, want both repositories", repoIDs(got))
	}
	// One repository in the set answers alone...
	if got := query(t, storage.QueryOpts{Name: name, Repos: []string{here}}); !equalStrings(repoIDs(got), []string{here}) {
		t.Errorf("scoped to one = %v, want only %s", repoIDs(got), here)
	}
	// ...and naming both is the same answer as naming neither.
	if got := query(t, storage.QueryOpts{Name: name, Repos: []string{here, there}}); len(got) != 2 {
		t.Errorf("scoped to both = %v, want both repositories", repoIDs(got))
	}
	// A set nothing matches is an empty answer, not an unfiltered one. This is
	// the failure mode worth pinning: a filter that degrades to "everywhere"
	// when it selects nothing answers a scoped question out of the whole
	// database.
	if got := query(t, storage.QueryOpts{Name: name, Repos: []string{newRepoID("absent")}}); len(got) != 0 {
		t.Errorf("scoped to a repository holding nothing = %v, want nothing", repoIDs(got))
	}
	// The set and the single repository narrow together.
	if got := query(t, storage.QueryOpts{RepoID: here, Name: name, Repos: []string{there}}); len(got) != 0 {
		t.Errorf("repo_id outside the set = %v, want nothing", repoIDs(got))
	}
	if got := query(t, storage.QueryOpts{RepoID: here, Name: name, Repos: []string{here, there}}); !equalStrings(repoIDs(got), []string{here}) {
		t.Errorf("repo_id inside the set = %v, want only %s", repoIDs(got), here)
	}

	// The fallback lookup runs a second statement when the first comes back
	// short, and that one is composed separately — so it is where a scope would
	// go missing. The near repository holds one match and the far one holds two;
	// scoped to the near one, the page stays at one however much room is left.
	near := query(t, storage.QueryOpts{NameOrQualified: name, Repos: []string{here}, Limit: 5, Fallback: true})
	if !equalStrings(repoIDs(near), []string{here}) {
		t.Errorf("fallback scoped to one repository = %v, want only %s", repoIDs(near), here)
	}
	far := query(t, storage.QueryOpts{NameOrQualified: name, Repos: []string{there}, Limit: 5, Fallback: true})
	if !equalStrings(repoIDs(far), []string{there, there}) {
		t.Errorf("fallback scoped to the far repository = %v, want its exact and substring matches", repoIDs(far))
	}
}

// testSymbolLookup covers what an agent's symbol query needs from the store:
// that it finds the symbol whatever case the question phrased it in, that one
// term can be either a name or a qualified name, that a miss comes back as a
// near match rather than as nothing, that the hand-written implementation
// outranks the generated stubs sharing its name, and that it keeps outranking
// them when only the stubs match the term exactly.
//
// These are one subtest because they are one query path, and the fixture is one
// the ranking has an opinion about: the generated units are stored first, which
// is how the corpus produced them and what an ordering by insertion id put in
// front of the answer.
func testSymbolLookup(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	repoID := newRepoID("symbols")
	seedRepo(t, st, repoID)

	unit := func(path, name, qualified string) *storage.ASTUnit {
		return &storage.ASTUnit{
			RepoID: repoID, FilePath: path, Language: "go", Kind: "function",
			Name: name, Qualified: qualified, StartLine: 1, EndLine: 2,
		}
	}
	units := []*storage.ASTUnit{
		unit("src/frontend/genproto/demo.pb.go", "ShipOrder", "pb.ShipOrder"),
		unit("protos/demo.proto", "ShipOrder", "demo.ShipOrder"),
		unit("src/shippingservice/main_test.go", "ShipOrder", "main.ShipOrderTest"),
		unit("src/shippingservice/main.go", "ShipOrder", "main.ShipOrder"),
		// A contract unit, whose qualified name is the bare key the linker
		// joins on (as in testEdgeResolution) rather than a package path. One
		// term reaches it through qualified while reaching the four above
		// through name.
		unit("src/shippingservice/service.go", "ShippingService", "ShipOrder"),
		unit("src/paymentservice/charge.go", "Charge", "payment.Charge"),
		unit("src/paymentservice/charge.go", "ChargeCard", "payment.ChargeCard"),
		unit("src/paymentservice/charge.go", "ChargeCardHandler", "payment.ChargeCardHandler"),
		// The boutique checkout flow, at the paths and names the corpus really
		// holds. PlaceOrder is declared once and generated into every service's
		// stubs, so a term naming it matches four files nobody wrote *exactly*,
		// while the frontend handler that actually starts a checkout only
		// contains it. Stored generated-first, as above.
		unit("protos/demo.proto", "PlaceOrder", "hipstershop.PlaceOrder"),
		unit("src/frontend/genproto/demo_grpc.pb.go", "PlaceOrder", "pb.PlaceOrder"),
		unit("src/checkoutservice/genproto/demo_grpc.pb.go", "PlaceOrder", "pb.PlaceOrder"),
		unit("src/emailservice/demo_pb2_grpc.py", "PlaceOrder", "demo_pb2.PlaceOrder"),
		unit("src/checkoutservice/main.go", "PlaceOrder", "main.PlaceOrder"),
		unit("src/frontend/handlers.go", "placeOrderHandler", "main.placeOrderHandler"),
	}
	if err := st.BatchStoreASTUnits(ctx, units); err != nil {
		t.Fatalf("BatchStoreASTUnits: %v", err)
	}

	paths := func(got []*storage.ASTUnit) []string {
		out := make([]string, 0, len(got))
		for _, u := range got {
			out = append(out, u.FilePath)
		}
		return out
	}
	names := func(got []*storage.ASTUnit) []string {
		out := make([]string, 0, len(got))
		for _, u := range got {
			out = append(out, u.Name)
		}
		return out
	}
	query := func(t *testing.T, opts storage.QueryOpts) []*storage.ASTUnit {
		t.Helper()
		opts.RepoID = repoID
		got, err := st.GetASTUnits(ctx, opts)
		if err != nil {
			t.Fatalf("GetASTUnits(%+v): %v", opts, err)
		}
		return got
	}

	// The lookup is case-insensitive. An agent writes the symbol the way the
	// question phrases it — "which code charges the card" gives "charge" —
	// and against an index holding "Charge" that used to return nothing.
	got := query(t, storage.QueryOpts{Name: "charge"})
	if len(got) != 1 || got[0].Name != "Charge" {
		t.Errorf(`GetASTUnits(name="charge") = %v, want the one unit named Charge`, names(got))
	}
	if up := query(t, storage.QueryOpts{Name: "CHARGE"}); len(up) != 1 {
		t.Errorf(`GetASTUnits(name="CHARGE") = %v, want the same one unit`, names(up))
	}
	if q := query(t, storage.QueryOpts{Qualified: "PAYMENT.charge"}); len(q) != 1 {
		t.Errorf("qualified matching is still case-sensitive: %v", names(q))
	}

	// One term against name *or* qualified widens: it returns both the units
	// carrying it as a name and the one carrying it as a qualified name, which
	// is more than either filter alone can reach.
	byName := query(t, storage.QueryOpts{Name: "shiporder"})
	byQualified := query(t, storage.QueryOpts{Qualified: "shiporder"})
	either := query(t, storage.QueryOpts{NameOrQualified: "shiporder"})
	if len(byName) != 4 || len(byQualified) != 1 || len(either) != 5 {
		t.Errorf("name=%d qualified=%d name-or-qualified=%d (%v); want 4, 1 and the 5 of both",
			len(byName), len(byQualified), len(either), paths(either))
	}
	// Name and Qualified together still narrow, which is what their existing
	// callers rely on.
	if both := query(t, storage.QueryOpts{Name: "ShippingService", Qualified: "ShipOrder"}); len(both) != 1 {
		t.Errorf("Name and Qualified together = %d units, want the 1 matching both", len(both))
	}
	if both := query(t, storage.QueryOpts{Name: "ShipOrder", Qualified: "ShipOrder"}); len(both) != 0 {
		t.Errorf("Name and Qualified must narrow together, got %d units", len(both))
	}

	// The hand-written implementation outranks the generated stubs and the test
	// that share its name — and it does so before the limit is applied, which
	// is the whole point: ordering by insertion id returned ten generated files
	// ahead of the only unit that answered the question.
	ranked := query(t, storage.QueryOpts{Name: "ShipOrder"})
	// The three penalized rows tie on everything the ranking has an opinion
	// about, so they come back in path order — the tie-break is where a unit is,
	// not when it was stored. Stored order here is pb.go, proto, main_test.go.
	wantOrder := []string{
		"src/shippingservice/main.go",
		"protos/demo.proto",
		"src/frontend/genproto/demo.pb.go",
		"src/shippingservice/main_test.go",
	}
	if !equalStrings(paths(ranked), wantOrder) {
		t.Errorf("ranking = %v, want %v", paths(ranked), wantOrder)
	}
	if top := query(t, storage.QueryOpts{Name: "ShipOrder", Limit: 1}); len(top) != 1 ||
		top[0].FilePath != "src/shippingservice/main.go" {
		t.Errorf("limit=1 returned %v, want only the hand-written implementation", paths(top))
	}

	// The fallback tier turns a miss into a near match. It is opt-in, it never
	// outranks an exact match, it does not repeat one, and it puts the most
	// precise candidate first.
	if plain := query(t, storage.QueryOpts{Name: "chargecar", Limit: 5}); len(plain) != 0 {
		t.Errorf("a substring matched without Fallback: %v", names(plain))
	}
	near := query(t, storage.QueryOpts{Name: "chargecar", Limit: 5, Fallback: true})
	if !equalStrings(names(near), []string{"ChargeCard", "ChargeCardHandler"}) {
		t.Errorf("fallback for a miss = %v, want ChargeCard then ChargeCardHandler", names(near))
	}
	topped := query(t, storage.QueryOpts{Name: "charge", Limit: 5, Fallback: true})
	if !equalStrings(names(topped), []string{"Charge", "ChargeCard", "ChargeCardHandler"}) {
		t.Errorf("topped-up page = %v, want the exact match first and no repeat of it", names(topped))
	}
	// A page the exact tier already filled is never widened.
	if full := query(t, storage.QueryOpts{Name: "charge", Limit: 1, Fallback: true}); len(full) != 1 ||
		full[0].Name != "Charge" {
		t.Errorf("a full page was widened: %v", names(full))
	}
	// The fallback reaches the qualified name through the one-term form too.
	viaQualified := query(t, storage.QueryOpts{NameOrQualified: "chargecardh", Limit: 5, Fallback: true})
	if !equalStrings(names(viaQualified), []string{"ChargeCardHandler"}) {
		t.Errorf("fallback on the one-term form = %v, want ChargeCardHandler", names(viaQualified))
	}

	// A generated exact match loses to a hand-written near miss. Four of the six
	// units matching "placeorder" match it as written and none of the four is
	// code anyone wrote; ranking exactness above the path penalty filled the
	// page with them and left the frontend handler — the one hand-written unit
	// the term merely occurs in — off it entirely. Its being on the page here,
	// ahead of every stub, is the whole point.
	//
	// Exactness still decides between two hand-written rows, which is why the
	// checkoutservice implementation is first and the handler second; what
	// changed is that it no longer decides across the generated/hand-written
	// line.
	checkout := query(t, storage.QueryOpts{NameOrQualified: "placeorder", Limit: 3, Fallback: true})
	if !equalStrings(paths(checkout), []string{
		"src/checkoutservice/main.go",
		"src/frontend/handlers.go",
		"protos/demo.proto",
	}) {
		t.Errorf("generated stubs outranked hand-written code: %v", paths(checkout))
	}
	// The stubs are demoted, not dropped: widen the page and they follow, in the
	// path order the ranking's tie-break puts them in — which is not the order
	// they were stored in (frontend, checkoutservice, emailservice).
	all := query(t, storage.QueryOpts{NameOrQualified: "placeorder", Limit: 10, Fallback: true})
	if !equalStrings(paths(all), []string{
		"src/checkoutservice/main.go",
		"src/frontend/handlers.go",
		"protos/demo.proto",
		"src/checkoutservice/genproto/demo_grpc.pb.go",
		"src/emailservice/demo_pb2_grpc.py",
		"src/frontend/genproto/demo_grpc.pb.go",
	}) {
		t.Errorf("full page = %v", paths(all))
	}
	// Without the fallback nothing about this lookup changed: exact matches
	// only, still ranked by path. The linker resolves edges through that query,
	// and a near miss there points an edge at the wrong symbol rather than
	// ranking one badly.
	exactOnly := query(t, storage.QueryOpts{NameOrQualified: "placeorder", Limit: 3})
	if !equalStrings(paths(exactOnly), []string{
		"src/checkoutservice/main.go",
		"protos/demo.proto",
		"src/checkoutservice/genproto/demo_grpc.pb.go",
	}) {
		t.Errorf("the lookup without Fallback changed: %v", paths(exactOnly))
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func testFiles(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	repoID := newRepoID("files")
	seedRepo(t, st, repoID)

	files := []*storage.File{
		{RepoID: repoID, Path: "a.go", Hash: "h1", Language: "go", Size: 10},
		{RepoID: repoID, Path: "b.go", Hash: "h2", Language: "go", Size: 20},
	}
	if err := st.BatchStoreFiles(ctx, files); err != nil {
		t.Fatalf("BatchStoreFiles: %v", err)
	}

	got, err := st.GetFile(ctx, repoID, "a.go")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if got.Hash != "h1" || got.Language != "go" || got.Size != 10 {
		t.Errorf("GetFile = %+v, want hash=h1 language=go size=10", got)
	}

	if _, err := st.GetFile(ctx, repoID, "nope.go"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("GetFile(missing) = %v, want storage.ErrNotFound", err)
	}

	// An upsert of the same path replaces the row rather than adding one.
	files[0].Hash = "h1b"
	if err := st.BatchStoreFiles(ctx, files[:1]); err != nil {
		t.Fatalf("BatchStoreFiles (upsert): %v", err)
	}
	list, err := st.GetFilesByRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("GetFilesByRepo: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("GetFilesByRepo after upsert = %d files, want 2", len(list))
	}
	if got, _ = st.GetFile(ctx, repoID, "a.go"); got.Hash != "h1b" {
		t.Errorf("hash after upsert = %q, want h1b", got.Hash)
	}

	if err := st.DeleteFilesByPaths(ctx, repoID, []string{"a.go"}); err != nil {
		t.Fatalf("DeleteFilesByPaths: %v", err)
	}
	if list, _ = st.GetFilesByRepo(ctx, repoID); len(list) != 1 {
		t.Errorf("after DeleteFilesByPaths = %d files, want 1", len(list))
	}

	if err := st.DeleteFilesByRepo(ctx, repoID); err != nil {
		t.Fatalf("DeleteFilesByRepo: %v", err)
	}
	if list, _ = st.GetFilesByRepo(ctx, repoID); len(list) != 0 {
		t.Errorf("after DeleteFilesByRepo = %d files, want 0", len(list))
	}
}

// testInsertionOrder pins the property the retrieval eval depends on and
// nothing else asserted: two passes over identical sources return identical
// results, in identical order.
//
// Indexing runs on indexes.workers goroutines, so which row reaches the store
// first is a property of the machine and not of the corpus. It used to be the
// store's last word on ranking — every unit query ended in `..., id` and every
// edge query in `ORDER BY id` — and the eval question "which entity maps to the
// visits table" scored rank 1 on six runs of identical code over an identical
// corpus and rank 3 on two, because the three declarations of `db:visits` tie on
// every term above the tie-break and the workers committed them in a different
// order. That fixture is this one, at the paths petclinic really holds: one
// entity class and two schema files, none of them generated or test code, all
// three named "visits".
//
// Storing the same rows in two orders under two repositories and demanding the
// same answer is the whole property; a limit shorter than the result is part of
// it, because insertion order decided which rows survived the cut and not
// merely how the survivors were arranged.
func testInsertionOrder(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	forward, reverse := newRepoID("order-fwd"), newRepoID("order-rev")
	seedRepo(t, st, forward)
	seedRepo(t, st, reverse)

	// Paths whose alphabetical order is not the order they are written in, so a
	// store that simply returned rows as stored would pass by accident.
	files := []struct {
		path string
		line int
	}{
		{"spring-petclinic-visits-service/src/main/resources/db/mysql/schema.sql", 5},
		{"spring-petclinic-visits-service/src/main/java/org/springframework/samples/petclinic/visits/model/Visit.java", 31},
		{"spring-petclinic-visits-service/src/main/resources/db/hsqldb/schema.sql", 3},
	}
	store := func(repoID string, reversed bool) {
		t.Helper()
		units := make([]*storage.ASTUnit, 0, len(files))
		edges := make([]*storage.Edge, 0, len(files))
		for i := range files {
			f := files[i]
			if reversed {
				f = files[len(files)-1-i]
			}
			units = append(units, &storage.ASTUnit{
				RepoID: repoID, FilePath: f.path, Language: "sql", Kind: "db_table",
				Name: "visits", Qualified: "db:visits", StartLine: f.line, EndLine: f.line,
			})
		}
		if err := st.BatchStoreASTUnits(ctx, units); err != nil {
			t.Fatalf("BatchStoreASTUnits(%s): %v", repoID, err)
		}
		// Each edge starts at the unit stored for the same file, so its src_id is
		// itself an insertion-order id — which is the point: the order the store
		// returns edges in must not follow it.
		for _, u := range units {
			edges = append(edges, &storage.Edge{
				RepoID: repoID, SrcID: u.ID, Kind: storage.EdgeWritesTo, DstName: "db:visits",
				FilePath: u.FilePath, Line: u.StartLine, Confidence: 0.63,
			})
		}
		if err := st.BatchStoreEdges(ctx, edges); err != nil {
			t.Fatalf("BatchStoreEdges(%s): %v", repoID, err)
		}
	}
	store(forward, false)
	store(reverse, true)

	unitPaths := func(repoID string, limit int) []string {
		t.Helper()
		got, err := st.GetASTUnits(ctx, storage.QueryOpts{
			RepoID: repoID, Qualified: "db:visits", Limit: limit,
		})
		if err != nil {
			t.Fatalf("GetASTUnits(%s): %v", repoID, err)
		}
		out := make([]string, 0, len(got))
		for _, u := range got {
			out = append(out, u.FilePath)
		}
		return out
	}
	edgePaths := func(repoID string, limit int) []string {
		t.Helper()
		got, err := st.GetEdges(ctx, storage.QueryOpts{
			RepoID: repoID, Name: "db:visits", Limit: limit,
		})
		if err != nil {
			t.Fatalf("GetEdges(%s): %v", repoID, err)
		}
		out := make([]string, 0, len(got))
		for _, e := range got {
			out = append(out, e.FilePath)
		}
		return out
	}

	for _, limit := range []int{0, 2} {
		if a, b := unitPaths(forward, limit), unitPaths(reverse, limit); !equalStrings(a, b) {
			t.Errorf("unit order depends on insertion order (limit %d):\n stored forward: %v\n stored reversed: %v",
				limit, a, b)
		}
		if a, b := edgePaths(forward, limit), edgePaths(reverse, limit); !equalStrings(a, b) {
			t.Errorf("edge order depends on insertion order (limit %d):\n stored forward: %v\n stored reversed: %v",
				limit, a, b)
		}
	}

	// And the order is the corpus's own: the entity class, whose path sorts
	// first, is what the question is answered with. Asserting the order as well
	// as its stability is what keeps a future tie-break from being stable and
	// arbitrary at the same time.
	want := []string{
		"spring-petclinic-visits-service/src/main/java/org/springframework/samples/petclinic/visits/model/Visit.java",
		"spring-petclinic-visits-service/src/main/resources/db/hsqldb/schema.sql",
		"spring-petclinic-visits-service/src/main/resources/db/mysql/schema.sql",
	}
	if got := unitPaths(forward, 0); !equalStrings(got, want) {
		t.Errorf("units = %v, want them in path order %v", got, want)
	}
	if got := edgePaths(forward, 0); !equalStrings(got, want) {
		t.Errorf("edges = %v, want them in path order %v", got, want)
	}
}

func testUnitsAndEdges(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	repoID := newRepoID("units")
	seedRepo(t, st, repoID)

	caller := &storage.ASTUnit{
		RepoID: repoID, FilePath: "caller.go", Language: "go", Kind: "function",
		Name: "Caller", Qualified: "pkg.Caller", StartLine: 1, EndLine: 5,
	}
	callee := &storage.ASTUnit{
		RepoID: repoID, FilePath: "callee.go", Language: "go", Kind: "function",
		Name: "Callee", Qualified: "pkg.Callee", StartLine: 1, EndLine: 3,
	}
	if err := st.BatchStoreASTUnits(ctx, []*storage.ASTUnit{caller, callee}); err != nil {
		t.Fatalf("BatchStoreASTUnits: %v", err)
	}
	if caller.ID == "" || callee.ID == "" {
		t.Fatalf("stored units carry no ids: caller=%q callee=%q", caller.ID, callee.ID)
	}

	if n, err := st.CountASTUnitsByRepo(ctx, repoID); err != nil || n != 2 {
		t.Errorf("CountASTUnitsByRepo = %d, %v; want 2, nil", n, err)
	}

	byKind, err := st.GetASTUnits(ctx, storage.QueryOpts{RepoID: repoID, Kind: "function"})
	if err != nil || len(byKind) != 2 {
		t.Errorf("GetASTUnits(kind=function) = %d units, %v; want 2, nil", len(byKind), err)
	}
	byFile, err := st.GetASTUnits(ctx, storage.QueryOpts{RepoID: repoID, FilePath: "callee.go"})
	if err != nil || len(byFile) != 1 {
		t.Fatalf("GetASTUnits(file=callee.go) = %d units, %v; want 1, nil", len(byFile), err)
	}

	one, err := st.GetASTUnitByID(ctx, callee.ID)
	if err != nil || one.Qualified != "pkg.Callee" {
		t.Errorf("GetASTUnitByID = %+v, %v", one, err)
	}

	edge := &storage.Edge{
		RepoID: repoID, SrcID: caller.ID, DstID: callee.ID, DstRepoID: repoID,
		Kind: storage.EdgeCall, DstName: "Callee", FilePath: "caller.go",
		Line: 2, Confidence: 0.9,
	}
	if err := st.BatchStoreEdges(ctx, []*storage.Edge{edge}); err != nil {
		t.Fatalf("BatchStoreEdges: %v", err)
	}
	edges, err := st.GetEdges(ctx, storage.QueryOpts{RepoID: repoID, SrcID: caller.ID})
	if err != nil || len(edges) != 1 {
		t.Fatalf("GetEdges(src) = %d edges, %v; want 1, nil", len(edges), err)
	}
	if edges[0].DstID != callee.ID {
		t.Errorf("edge dst = %q, want %q", edges[0].DstID, callee.ID)
	}

	// The contract DeleteASTUnitsByFile documents, and the one the linker's
	// idempotence depends on: deleting a file's units unresolves the edges that
	// pointed at them, in the same transaction. An edge that kept a stale id
	// would look resolved to every caller testing dst_id != "" while resolving
	// to nothing.
	if err := st.DeleteASTUnitsByFile(ctx, repoID, "callee.go"); err != nil {
		t.Fatalf("DeleteASTUnitsByFile: %v", err)
	}
	edges, err = st.GetEdges(ctx, storage.QueryOpts{RepoID: repoID, SrcID: caller.ID})
	if err != nil || len(edges) != 1 {
		t.Fatalf("GetEdges after unit delete = %d edges, %v; want the edge to survive", len(edges), err)
	}
	if edges[0].DstID != "" || edges[0].DstRepoID != "" {
		t.Errorf("edge still resolved after its destination was deleted: dst=%q dst_repo=%q",
			edges[0].DstID, edges[0].DstRepoID)
	}
	if edges[0].DstName != "Callee" {
		t.Errorf("unresolve changed the join key: dst_name = %q, want Callee", edges[0].DstName)
	}

	// Only the deleted file's edges are unresolved. A delete that cleared every
	// edge of the repository would hand the linker work it has already done,
	// and a resolution it cannot always reproduce.
	kept := &storage.ASTUnit{
		RepoID: repoID, FilePath: "kept.go", Language: "go", Kind: "function",
		Name: "Kept", Qualified: "pkg.Kept", StartLine: 1, EndLine: 3,
	}
	if err := st.BatchStoreASTUnits(ctx, []*storage.ASTUnit{kept}); err != nil {
		t.Fatalf("BatchStoreASTUnits (kept): %v", err)
	}
	keptEdge := &storage.Edge{
		RepoID: repoID, SrcID: caller.ID, DstID: kept.ID, DstRepoID: repoID,
		Kind: storage.EdgeCall, DstName: "Kept", FilePath: "caller.go", Line: 4, Confidence: 1,
	}
	if err := st.BatchStoreEdges(ctx, []*storage.Edge{keptEdge}); err != nil {
		t.Fatalf("BatchStoreEdges (kept): %v", err)
	}
	if err := st.DeleteASTUnitsByFiles(ctx, repoID, []string{"gone.go"}); err != nil {
		t.Fatalf("DeleteASTUnitsByFiles: %v", err)
	}
	after, err := st.GetEdges(ctx, storage.QueryOpts{RepoID: repoID, SrcID: caller.ID, Kind: storage.EdgeCall})
	if err != nil {
		t.Fatalf("GetEdges after unrelated delete: %v", err)
	}
	var found bool
	for _, e := range after {
		if e.DstName == "Kept" {
			found = true
			if e.DstID != kept.ID {
				t.Errorf("deleting another file unresolved an unrelated edge: dst = %q, want %q", e.DstID, kept.ID)
			}
		}
	}
	if !found {
		t.Error("the edge into the untouched file disappeared")
	}
}

func testEdgeResolution(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	repoID := newRepoID("resolve")
	seedRepo(t, st, repoID)

	src := &storage.ASTUnit{
		RepoID: repoID, FilePath: "src.go", Language: "go", Kind: "function",
		Name: "Src", Qualified: "pkg.Src", StartLine: 1, EndLine: 2,
	}
	dst := &storage.ASTUnit{
		RepoID: repoID, FilePath: "dst.go", Language: "go", Kind: storage.KindHTTPRoute,
		Name: "GET /things", Qualified: "http:GET /things", StartLine: 1, EndLine: 2,
	}
	if err := st.BatchStoreASTUnits(ctx, []*storage.ASTUnit{src, dst}); err != nil {
		t.Fatalf("BatchStoreASTUnits: %v", err)
	}

	edge := &storage.Edge{
		RepoID: repoID, SrcID: src.ID, Kind: storage.EdgeHTTPCall,
		DstName: "http:GET /things", FilePath: "src.go", Line: 3,
		Confidence: 0.9, Meta: `{"base_conf":0.9}`,
	}
	if err := st.BatchStoreEdges(ctx, []*storage.Edge{edge}); err != nil {
		t.Fatalf("BatchStoreEdges: %v", err)
	}
	stored, err := st.GetEdges(ctx, storage.QueryOpts{RepoID: repoID, Kind: storage.EdgeHTTPCall})
	if err != nil || len(stored) != 1 {
		t.Fatalf("GetEdges = %d edges, %v; want 1, nil", len(stored), err)
	}
	edgeID := stored[0].ID

	// Unresolved edges are findable as such: this is how the linker picks up
	// its work.
	unresolved, err := st.GetEdges(ctx, storage.QueryOpts{RepoID: repoID, Unresolved: true})
	if err != nil || len(unresolved) != 1 {
		t.Errorf("GetEdges(unresolved) = %d edges, %v; want 1, nil", len(unresolved), err)
	}

	if err := st.UpdateEdgeResolution(ctx, edgeID, dst.ID, repoID, 0.72); err != nil {
		t.Fatalf("UpdateEdgeResolution: %v", err)
	}
	stored, _ = st.GetEdges(ctx, storage.QueryOpts{RepoID: repoID, Kind: storage.EdgeHTTPCall})
	if stored[0].DstID != dst.ID || stored[0].DstRepoID != repoID {
		t.Errorf("after resolution dst = %q@%q, want %q@%q", stored[0].DstID, stored[0].DstRepoID, dst.ID, repoID)
	}
	if stored[0].Confidence < 0.71 || stored[0].Confidence > 0.73 {
		t.Errorf("confidence = %v, want 0.72", stored[0].Confidence)
	}

	// Annotating an edge rewrites its meta in place and leaves the join key —
	// and the resolution — untouched.
	if err := st.UpdateEdgeMeta(ctx, edgeID, `{"base_conf":0.9,"source":"llm"}`); err != nil {
		t.Fatalf("UpdateEdgeMeta: %v", err)
	}
	stored, _ = st.GetEdges(ctx, storage.QueryOpts{RepoID: repoID, Kind: storage.EdgeHTTPCall})
	if got := storage.DecodeEdgeMeta(stored[0].Meta); got.Source != "llm" || got.BaseConf != 0.9 {
		t.Errorf("meta after update = %+v, want source=llm base_conf=0.9", got)
	}
	if stored[0].DstName != "http:GET /things" || stored[0].DstID != dst.ID {
		t.Errorf("UpdateEdgeMeta disturbed the edge: dst_name=%q dst=%q", stored[0].DstName, stored[0].DstID)
	}

	// Batched resolution is optional (a backend may not implement it), so the
	// linker type-asserts for it; when it is there it must behave like the
	// single-edge path.
	batcher, ok := st.(storage.EdgeResolutionBatcher)
	if !ok {
		t.Log("backend does not implement EdgeResolutionBatcher; skipping the batch path")
		return
	}
	failures, err := batcher.BatchUpdateEdgeResolutions(ctx, []storage.EdgeResolution{
		{EdgeID: edgeID, DstID: "", DstRepoID: "", Confidence: 0.5},
	})
	if err != nil {
		t.Fatalf("BatchUpdateEdgeResolutions: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("BatchUpdateEdgeResolutions reported failures: %+v", failures)
	}
	stored, _ = st.GetEdges(ctx, storage.QueryOpts{RepoID: repoID, Kind: storage.EdgeHTTPCall})
	if stored[0].DstID != "" {
		t.Errorf("an empty DstID must clear the resolution, got %q", stored[0].DstID)
	}
}

func testCoverage(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	repoID := newRepoID("coverage")
	seedRepo(t, st, repoID)

	if _, err := st.GetRepoCoverage(ctx, repoID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("GetRepoCoverage before any pass = %v, want storage.ErrNotFound", err)
	}

	first := &storage.RepoCoverage{
		RepoID:    repoID,
		UpdatedAt: 100,
		Kinds: map[string]storage.CoverageCounts{
			storage.ContractKindHTTP: {Candidates: 10, Edges: 7},
			storage.ContractKindDB:   {Candidates: 4, Edges: 4},
		},
	}
	if err := st.StoreRepoCoverage(ctx, first); err != nil {
		t.Fatalf("StoreRepoCoverage: %v", err)
	}
	got, err := st.GetRepoCoverage(ctx, repoID)
	if err != nil {
		t.Fatalf("GetRepoCoverage: %v", err)
	}
	if len(got.Kinds) != 2 || got.Kinds[storage.ContractKindHTTP].Edges != 7 {
		t.Errorf("coverage = %+v, want two kinds with http edges=7", got.Kinds)
	}

	// A later pass replaces the summary: a kind it no longer reports must not
	// survive from the previous one.
	if err := st.StoreRepoCoverage(ctx, &storage.RepoCoverage{
		RepoID:    repoID,
		UpdatedAt: 200,
		Kinds:     map[string]storage.CoverageCounts{storage.ContractKindHTTP: {Candidates: 12, Edges: 12}},
	}); err != nil {
		t.Fatalf("StoreRepoCoverage (replace): %v", err)
	}
	got, err = st.GetRepoCoverage(ctx, repoID)
	if err != nil {
		t.Fatalf("GetRepoCoverage after replace: %v", err)
	}
	if len(got.Kinds) != 1 {
		t.Errorf("kinds after replace = %+v, want only http", got.Kinds)
	}
	if _, stale := got.Kinds[storage.ContractKindDB]; stale {
		t.Error("a kind absent from the new summary survived the replace")
	}

	if err := st.DeleteRepoCoverage(ctx, repoID); err != nil {
		t.Fatalf("DeleteRepoCoverage: %v", err)
	}
	if _, err := st.GetRepoCoverage(ctx, repoID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("GetRepoCoverage after delete = %v, want storage.ErrNotFound", err)
	}
}

func testJobs(t *testing.T, st storage.Storage) {
	ctx := context.Background()
	repoID := newRepoID("jobs")
	seedRepo(t, st, repoID)

	job, err := st.EnqueueIndexJob(ctx, repoID, false)
	if err != nil {
		t.Fatalf("EnqueueIndexJob: %v", err)
	}
	if job.Status != storage.JobStatusPending || job.Kind != storage.JobKindIndex {
		t.Errorf("job = %+v, want pending index job", job)
	}

	// Index jobs for one repo are interchangeable, so a second request is
	// absorbed by the pending one — and a forced reindex is never downgraded.
	again, err := st.EnqueueIndexJob(ctx, repoID, true)
	if err != nil {
		t.Fatalf("EnqueueIndexJob (second): %v", err)
	}
	if again.ID != job.ID {
		t.Errorf("second index job id = %q, want the pending one %q", again.ID, job.ID)
	}
	if !again.Force {
		t.Error("force was lost when the request was absorbed")
	}

	fetched, err := st.GetIndexJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetIndexJob: %v", err)
	}
	if fetched.RepoID != repoID || !fetched.Force {
		t.Errorf("GetIndexJob = %+v, want repo=%s force=true", fetched, repoID)
	}

	// Commit jobs each carry a distinct piece of history, so they never merge.
	c1, err := st.EnqueueCommitJob(ctx, repoID, `{"commits":["a"]}`)
	if err != nil {
		t.Fatalf("EnqueueCommitJob: %v", err)
	}
	c2, err := st.EnqueueCommitJob(ctx, repoID, `{"commits":["b"]}`)
	if err != nil {
		t.Fatalf("EnqueueCommitJob (second): %v", err)
	}
	if c1.ID == c2.ID {
		t.Error("two commit jobs merged; each batch must be its own job")
	}

	// The read paths never select the payload: it can be tens of megabytes.
	if got, err := st.GetIndexJob(ctx, c1.ID); err != nil || got.Payload != "" {
		t.Errorf("GetIndexJob payload = %q, %v; want empty", got.Payload, err)
	}

	list, err := st.ListIndexJobs(ctx, repoID, 10)
	if err != nil {
		t.Fatalf("ListIndexJobs: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("ListIndexJobs = %d jobs, want 3 (one index, two commit)", len(list))
	}
	for _, j := range list {
		if j.Payload != "" {
			t.Errorf("ListIndexJobs returned a payload for job %s", j.ID)
		}
	}
}
