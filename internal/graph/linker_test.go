package graph

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

func TestLinkerResolvesRPCCallAcrossRepos(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	caller := storeFunc(t, st, "repoA", "PlaceOrder", "(userID string)")
	rpcMethod := storeUnit(t, st, &storage.ASTUnit{
		RepoID: "repoB", FilePath: "proto/orders.proto", Language: "proto",
		Kind: storage.KindRPCMethod, Name: "CreateOrder",
		Qualified: "grpc:orders.OrderService/CreateOrder",
	})
	edge := storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: caller.ID,
		Kind: storage.EdgeRPCCall, DstName: "grpc:OrderService/CreateOrder",
	})

	if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}

	got := edgeByID(t, st, storage.EdgeRPCCall, edge.ID)
	if got.DstID != rpcMethod.ID {
		t.Errorf("DstID = %q, want %q", got.DstID, rpcMethod.ID)
	}
	if got.DstRepoID != "repoB" {
		t.Errorf("DstRepoID = %q, want repoB", got.DstRepoID)
	}
	if got.Confidence <= 0 {
		t.Errorf("confidence = %v, want > 0", got.Confidence)
	}
}

func TestLinkerDerivesKafkaFlow(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	producer := storeFunc(t, st, "repoA", "PublishOrder", "(order Order)")
	consumer := storeFunc(t, st, "repoB", "HandleOrder", "(msg Message)")
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: producer.ID,
		Kind: storage.EdgeProduces, DstName: "topic:orders.created",
	})
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoB", SrcID: consumer.ID,
		Kind: storage.EdgeConsumes, DstName: "topic:orders.created",
	})

	if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}

	flows, err := st.GetEdges(ctx, storage.QueryOpts{Kind: storage.EdgeKafkaFlow})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Fatalf("kafka_flow edges = %d, want 1", len(flows))
	}
	f := flows[0]
	if f.SrcID != producer.ID || f.DstID != consumer.ID {
		t.Errorf("flow = %s -> %s, want %s -> %s", f.SrcID, f.DstID, producer.ID, consumer.ID)
	}
	if f.DstRepoID != "repoB" || f.DstName != "topic:orders.created" {
		t.Errorf("flow dst = %q %q, want repoB topic:orders.created", f.DstRepoID, f.DstName)
	}
	if f.Confidence <= 0 {
		t.Errorf("flow confidence = %v, want > 0", f.Confidence)
	}

	// Re-running must not duplicate derived flows.
	if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}
	flows, err = st.GetEdges(ctx, storage.QueryOpts{Kind: storage.EdgeKafkaFlow})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Errorf("kafka_flow edges after rerun = %d, want 1", len(flows))
	}
}

func TestLinkerResolvesConfigTopicRef(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	producer := storeFunc(t, st, "repoA", "PublishOrder", "(order Order)")
	consumer := storeFunc(t, st, "repoB", "HandleOrder", "(msg Message)")
	storeUnit(t, st, &storage.ASTUnit{
		RepoID: "repoA", FilePath: ".env", Language: "env",
		Kind: storage.KindConfigKey, Name: "ORDERS_TOPIC",
		Qualified: "config:ORDERS_TOPIC", Signature: "orders.created",
	})
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: producer.ID,
		Kind: storage.EdgeProduces, DstName: "topic:${ORDERS_TOPIC}",
	})
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoB", SrcID: consumer.ID,
		Kind: storage.EdgeConsumes, DstName: "topic:orders.created",
	})

	if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}

	got := edgeBySrc(t, st, storage.EdgeProduces, producer.ID)
	if got.DstName != "topic:orders.created" {
		t.Errorf("produces DstName = %q, want topic:orders.created", got.DstName)
	}
	if ref := metaField(got.Meta, metaKeyTopicRef); ref != "ORDERS_TOPIC" {
		t.Errorf("produces meta %s = %q, want ORDERS_TOPIC", metaKeyTopicRef, ref)
	}

	flows, err := st.GetEdges(ctx, storage.QueryOpts{Kind: storage.EdgeKafkaFlow})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Fatalf("kafka_flow edges = %d, want 1", len(flows))
	}
	if flows[0].SrcID != producer.ID || flows[0].DstID != consumer.ID {
		t.Errorf("flow = %s -> %s, want %s -> %s",
			flows[0].SrcID, flows[0].DstID, producer.ID, consumer.ID)
	}
}

// TestLinkerReresolvesChangedConfigValue: the reference survives in the edge
// meta, so a later config change re-points the edge and rebuilds the flow.
func TestLinkerReresolvesChangedConfigValue(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	producer := storeFunc(t, st, "repoA", "PublishOrder", "(order Order)")
	oldConsumer := storeFunc(t, st, "repoB", "HandleV1", "(msg Message)")
	newConsumer := storeFunc(t, st, "repoB", "HandleV2", "(msg Message)")
	storeConfigValue := func(value string) {
		t.Helper()
		if err := st.DeleteASTUnitsByFile(ctx, "repoC", "app.yaml"); err != nil {
			t.Fatal(err)
		}
		storeUnit(t, st, &storage.ASTUnit{
			RepoID: "repoC", FilePath: "app.yaml", Language: "yaml",
			Kind: storage.KindConfigKey, Name: "orders-topic",
			Qualified: "config:kafka.orders-topic", Signature: value,
		})
	}
	storeConfigValue("orders.v1")
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: producer.ID,
		Kind: storage.EdgeProduces, DstName: "topic:${ORDERS_TOPIC}",
	})
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoB", SrcID: oldConsumer.ID,
		Kind: storage.EdgeConsumes, DstName: "topic:orders.v1",
	})
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoB", SrcID: newConsumer.ID,
		Kind: storage.EdgeConsumes, DstName: "topic:orders.v2",
	})

	l := NewLinker(st)
	if err := l.Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}
	if got := edgeBySrc(t, st, storage.EdgeProduces, producer.ID); got.DstName != "topic:orders.v1" {
		t.Fatalf("produces DstName = %q, want topic:orders.v1", got.DstName)
	}

	// The config value changes and only the config repo is reindexed: the
	// producer lives in repoA, so its topic is not in repoC's own edge set.
	storeConfigValue("orders.v2")
	if err := l.Run(ctx, "repoC"); err != nil {
		t.Fatal(err)
	}

	got := edgeBySrc(t, st, storage.EdgeProduces, producer.ID)
	if got.DstName != "topic:orders.v2" {
		t.Errorf("produces DstName after config change = %q, want topic:orders.v2", got.DstName)
	}
	flows, err := st.GetEdges(ctx, storage.QueryOpts{Kind: storage.EdgeKafkaFlow})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Fatalf("kafka_flow edges = %d, want 1: %+v", len(flows), flows)
	}
	if flows[0].DstID != newConsumer.ID {
		t.Errorf("flow consumer = %s, want %s (the v2 consumer)", flows[0].DstID, newConsumer.ID)
	}
}

// TestLinkerRewritesCollidingTopicGroups: one reference resolves onto the key
// another rewritten group is leaving, so the groups must all be deleted before
// any is re-inserted — otherwise the second delete takes the first group's
// fresh edges with it.
func TestLinkerRewritesCollidingTopicGroups(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	moving := storeFunc(t, st, "repoA", "PublishMoving", "(order Order)")
	arriving := storeFunc(t, st, "repoA", "PublishArriving", "(order Order)")
	storeUnit(t, st, &storage.ASTUnit{
		RepoID: "repoA", FilePath: ".env", Language: "env",
		Kind: storage.KindConfigKey, Name: "MOVING_TOPIC",
		Qualified: "config:MOVING_TOPIC", Signature: "orders.v2",
	})
	storeUnit(t, st, &storage.ASTUnit{
		RepoID: "repoA", FilePath: ".env", Language: "env",
		Kind: storage.KindConfigKey, Name: "ARRIVING_TOPIC",
		Qualified: "config:ARRIVING_TOPIC", Signature: "orders.v1",
	})
	// Already resolved to orders.v1 by an earlier run; its value moved to v2.
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: moving.ID, Kind: storage.EdgeProduces,
		DstName: "topic:orders.v1",
		Meta:    metaWithField("", metaKeyTopicRef, "MOVING_TOPIC"),
	})
	// Resolves for the first time onto the key the edge above is vacating.
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: arriving.ID, Kind: storage.EdgeProduces,
		DstName: "topic:${ARRIVING_TOPIC}",
	})

	if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}

	if got := edgeBySrc(t, st, storage.EdgeProduces, moving.ID); got.DstName != "topic:orders.v2" {
		t.Errorf("moving producer DstName = %q, want topic:orders.v2", got.DstName)
	}
	if got := edgeBySrc(t, st, storage.EdgeProduces, arriving.ID); got.DstName != "topic:orders.v1" {
		t.Errorf("arriving producer DstName = %q, want topic:orders.v1", got.DstName)
	}
}

// TestLinkerFallsBackToTopicDefault: "${KEY:default}" placeholders arrive with
// the default in the edge meta; it answers when no config key matches.
func TestLinkerFallsBackToTopicDefault(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	producer := storeFunc(t, st, "repoA", "PublishOrder", "(order Order)")
	consumer := storeFunc(t, st, "repoB", "HandleOrder", "(msg Message)")
	meta, err := json.Marshal(&storage.EdgeMeta{Topic: "orders.created"})
	if err != nil {
		t.Fatal(err)
	}
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: producer.ID,
		Kind: storage.EdgeProduces, DstName: "topic:${orders.topic}", Meta: string(meta),
	})
	storeEdge(t, st, &storage.Edge{
		RepoID: "repoB", SrcID: consumer.ID,
		Kind: storage.EdgeConsumes, DstName: "topic:orders.created",
	})

	if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}

	got := edgeBySrc(t, st, storage.EdgeProduces, producer.ID)
	if got.DstName != "topic:orders.created" {
		t.Errorf("produces DstName = %q, want the meta default topic:orders.created", got.DstName)
	}
	flows, err := st.GetEdges(ctx, storage.QueryOpts{Kind: storage.EdgeKafkaFlow})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Errorf("kafka_flow edges = %d, want 1", len(flows))
	}
}

func TestLinkerResolvesHTTPCallWithTemplates(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	client := storeFunc(t, st, "repoA", "FetchX", "(id string)")
	route := storeUnit(t, st, &storage.ASTUnit{
		RepoID: "repoB", FilePath: "server/routes.go", Language: "go",
		Kind: storage.KindHTTPRoute, Name: "POST /api/x/{param}",
		Qualified: "http:POST /api/x/{param}",
	})
	edge := storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: client.ID,
		Kind: storage.EdgeHTTPCall, DstName: "http:POST /api/x/{id}",
	})

	if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}

	got := edgeByID(t, st, storage.EdgeHTTPCall, edge.ID)
	if got.DstID != route.ID {
		t.Errorf("DstID = %q, want route %q", got.DstID, route.ID)
	}
	if got.DstRepoID != "repoB" {
		t.Errorf("DstRepoID = %q, want repoB", got.DstRepoID)
	}
	if got.Confidence <= 0 {
		t.Errorf("confidence = %v, want > 0", got.Confidence)
	}
}

func TestLinkerResolvesWritesToTable(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	fn := storeFunc(t, st, "repoA", "SaveOrder", "(order Order)")
	table := storeUnit(t, st, &storage.ASTUnit{
		RepoID: "repoA", FilePath: "migrations/001.sql", Language: "sql",
		Kind: storage.KindDBTable, Name: "orders", Qualified: "db:orders",
	})
	edge := storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: fn.ID,
		Kind: storage.EdgeWritesTo, DstName: "db:orders",
	})

	if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}

	got := edgeByID(t, st, storage.EdgeWritesTo, edge.ID)
	if got.DstID != table.ID {
		t.Errorf("DstID = %q, want table %q", got.DstID, table.ID)
	}
	if got.DstRepoID != "repoA" {
		t.Errorf("DstRepoID = %q, want repoA", got.DstRepoID)
	}
	if got.Confidence <= 0 {
		t.Errorf("confidence = %v, want > 0", got.Confidence)
	}
}

// TestLinkerJoinsORMWriteToRenamedTable: the ORM detector keys the write from
// the entity name (db:catalog_items) while the mapping declares the table under
// another one (EF Core's ToTable("Catalog")). The entity in the table unit's
// signature is what joins the two halves.
func TestLinkerJoinsORMWriteToRenamedTable(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	fn := storeFunc(t, st, "repoA", "AddItem", "(item CatalogItem)")
	table := storeUnit(t, st, &storage.ASTUnit{
		RepoID: "repoA", FilePath: "Infrastructure/CatalogItemConfiguration.cs", Language: "csharp",
		Kind: storage.KindDBTable, Name: "catalog", Qualified: "db:catalog",
		Signature: "entity:CatalogItem",
	})
	edge := storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: fn.ID,
		Kind: storage.EdgeWritesTo, DstName: "db:catalog_items",
	})

	if err := NewLinker(st).Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}

	got := edgeByID(t, st, storage.EdgeWritesTo, edge.ID)
	if got.DstID != table.ID {
		t.Errorf("DstID = %q, want table %q", got.DstID, table.ID)
	}
	if got.DstRepoID != "repoA" {
		t.Errorf("DstRepoID = %q, want repoA", got.DstRepoID)
	}
	if got.Confidence <= 0 {
		t.Errorf("confidence = %v, want > 0", got.Confidence)
	}
}

// TestLinkerPrefersReceiverForCallEdge: two types declare Save; the edge meta
// names the receiver, so the call must not land on the other one.
func TestLinkerPrefersReceiverForCallEdge(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	caller := storeFunc(t, st, "r1", "Handle", "(order Order)")
	userSave := storeUnit(t, st, &storage.ASTUnit{
		RepoID: "r1", FilePath: "src/user_repo.go", Language: "go",
		Kind: "method", Name: "Save", Qualified: "repo.UserRepo.Save",
		Signature: "(u User)",
	})
	orderSave := storeUnit(t, st, &storage.ASTUnit{
		RepoID: "r1", FilePath: "src/order_repo.go", Language: "go",
		Kind: "method", Name: "Save", Qualified: "repo.OrderRepo.Save",
		Signature: "(o Order)",
	})
	meta, err := json.Marshal(map[string]any{"args": []string{"order"}, metaKeyRecvType: "OrderRepo"})
	if err != nil {
		t.Fatal(err)
	}
	edge := storeEdge(t, st, &storage.Edge{
		RepoID: "r1", SrcID: caller.ID, Kind: storage.EdgeCall,
		DstName: "Save", FilePath: caller.FilePath, Line: 3, Meta: string(meta),
	})

	if err := NewLinker(st).Run(ctx, "r1"); err != nil {
		t.Fatal(err)
	}

	got := edgeByID(t, st, storage.EdgeCall, edge.ID)
	if got.DstID != orderSave.ID {
		t.Errorf("DstID = %q, want OrderRepo.Save (%s), not UserRepo.Save (%s)",
			got.DstID, orderSave.ID, userSave.ID)
	}
	if got.Confidence != 1.0*contract.ConfCrossFile {
		t.Errorf("confidence = %v, want ConfCrossFile for a receiver match in another file", got.Confidence)
	}
}

// TestLinkerNameOnlyCallIsHeuristic: with several same-named definitions and
// nothing to choose by, the pick is a guess and must be scored as one.
func TestLinkerNameOnlyCallIsHeuristic(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	caller := storeFunc(t, st, "r1", "Handle", "(order Order)")
	for _, qual := range []string{"repo.UserRepo.Save", "repo.OrderRepo.Save"} {
		storeUnit(t, st, &storage.ASTUnit{
			RepoID: "r1", FilePath: "src/" + qual + ".go", Language: "go",
			Kind: "method", Name: "Save", Qualified: qual,
		})
	}
	edge := storeEdge(t, st, &storage.Edge{
		RepoID: "r1", SrcID: caller.ID, Kind: storage.EdgeCall,
		DstName: "Save", FilePath: caller.FilePath, Line: 3,
	})

	if err := NewLinker(st).Run(ctx, "r1"); err != nil {
		t.Fatal(err)
	}

	got := edgeByID(t, st, storage.EdgeCall, edge.ID)
	if got.Confidence != 1.0*contract.ConfHeuristic {
		t.Errorf("confidence = %v, want ConfHeuristic for a name-only guess", got.Confidence)
	}
}

// TestLinkerClearsStaleResolution: when the contract unit an edge points at is
// gone, the edge must be unresolved again rather than keep a dangling dst_id.
func TestLinkerClearsStaleResolution(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	client := storeFunc(t, st, "repoA", "FetchX", "(id string)")
	route := storeUnit(t, st, &storage.ASTUnit{
		RepoID: "repoB", FilePath: "server/routes.go", Language: "go",
		Kind: storage.KindHTTPRoute, Name: "GET /api/x", Qualified: "http:GET /api/x",
	})
	edge := storeEdge(t, st, &storage.Edge{
		RepoID: "repoA", SrcID: client.ID,
		Kind: storage.EdgeHTTPCall, DstName: "http:GET /api/x",
	})

	l := NewLinker(st)
	if err := l.Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}
	if got := edgeByID(t, st, storage.EdgeHTTPCall, edge.ID); got.DstID != route.ID {
		t.Fatalf("DstID = %q, want %q", got.DstID, route.ID)
	}

	if err := st.DeleteASTUnitsByRepo(ctx, "repoB"); err != nil {
		t.Fatal(err)
	}
	if err := l.Run(ctx, "repoA"); err != nil {
		t.Fatal(err)
	}

	got := edgeByID(t, st, storage.EdgeHTTPCall, edge.ID)
	if got.DstID != "" || got.DstRepoID != "" {
		t.Errorf("edge dst = %q@%q, want it cleared once the route disappeared", got.DstID, got.DstRepoID)
	}
}

// edgeBySrc fetches the single edge of the given kind leaving srcID. Edge ids
// change whenever the linker rewrites an edge group.
func edgeBySrc(t *testing.T, st storage.Storage, kind, srcID string) *storage.Edge {
	t.Helper()
	edges, err := st.GetEdges(context.Background(), storage.QueryOpts{Kind: kind, SrcID: srcID})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("%s edges from %s = %d, want 1", kind, srcID, len(edges))
	}
	return edges[0]
}

// edgeByID fetches an edge of the given kind by its ID.
func edgeByID(t *testing.T, st storage.Storage, kind, id string) *storage.Edge {
	t.Helper()
	edges, err := st.GetEdges(context.Background(), storage.QueryOpts{Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("edge %s of kind %s not found", id, kind)
	return nil
}
