package ast

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

func TestGoCallEdgeCarriesAliases(t *testing.T) {
	src := `package main

func f(userID string) {
	x := userID
	g(x)
}

func g(uid string) {}
`
	_, edges := parseOrFail(t, "go", "main.go", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g; edges: %+v", edgeNames(edges))
	}
	meta := storage.DecodeEdgeMeta(e.Meta)
	if got := meta.Aliases["x"]; got != "userID" {
		t.Errorf("Aliases[x] = %q, want %q (aliases: %v)", got, "userID", meta.Aliases)
	}
	if len(meta.Args) != 1 || meta.Args[0] != "x" {
		t.Errorf("Args = %v, want [x]", meta.Args)
	}
}

func TestGoCallEdgeCarriesSelectorAlias(t *testing.T) {
	src := `package main

func f(body Request) {
	x := body.UserID
	g(x)
}
`
	_, edges := parseOrFail(t, "go", "main.go", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g; edges: %+v", edgeNames(edges))
	}
	if got := storage.DecodeEdgeMeta(e.Meta).Aliases["x"]; got != "body.UserID" {
		t.Errorf("Aliases[x] = %q, want %q", got, "body.UserID")
	}
}

func TestGoCallEdgeOmitsIrrelevantAliases(t *testing.T) {
	src := `package main

func f(userID, orderID string) {
	x := userID
	y := orderID
	g(x)
}
`
	_, edges := parseOrFail(t, "go", "main.go", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g")
	}
	meta := storage.DecodeEdgeMeta(e.Meta)
	if got := meta.Aliases["x"]; got != "userID" {
		t.Errorf("Aliases[x] = %q, want userID", got)
	}
	if _, ok := meta.Aliases["y"]; ok {
		t.Errorf("alias y is not referenced by the call args, must not be attached: %v", meta.Aliases)
	}
}

func TestPythonCallEdgeCarriesAliases(t *testing.T) {
	src := `def f(user_id):
    x = user_id
    g(x)
`
	_, edges := parseOrFail(t, "python", "main.py", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g; edges: %+v", edgeNames(edges))
	}
	if got := storage.DecodeEdgeMeta(e.Meta).Aliases["x"]; got != "user_id" {
		t.Errorf("Aliases[x] = %q, want %q", got, "user_id")
	}
}

func TestTypeScriptCallEdgeCarriesAliases(t *testing.T) {
	src := `function f(userId: string) {
	const x = userId;
	g(x);
}
`
	_, edges := parseOrFail(t, "typescript", "main.ts", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g; edges: %+v", edgeNames(edges))
	}
	if got := storage.DecodeEdgeMeta(e.Meta).Aliases["x"]; got != "userId" {
		t.Errorf("Aliases[x] = %q, want %q", got, "userId")
	}
}

func TestJavaCallEdgeCarriesAliases(t *testing.T) {
	src := `package com.example;

class Svc {
	void f(String userId) {
		String x = userId;
		g(x);
	}
}
`
	_, edges := parseOrFail(t, "java", "Svc.java", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g; edges: %+v", edgeNames(edges))
	}
	if got := storage.DecodeEdgeMeta(e.Meta).Aliases["x"]; got != "userId" {
		t.Errorf("Aliases[x] = %q, want %q", got, "userId")
	}
}

func TestCSharpCallEdgeCarriesAliases(t *testing.T) {
	src := `namespace App;

class Svc {
	void F(string userId) {
		var x = userId;
		G(x);
	}
}
`
	_, edges := parseOrFail(t, "csharp", "Svc.cs", src)

	e := findEdge(edges, storage.EdgeCall, "G")
	if e == nil {
		t.Fatalf("missing call edge to G; edges: %+v", edgeNames(edges))
	}
	if got := storage.DecodeEdgeMeta(e.Meta).Aliases["x"]; got != "userId" {
		t.Errorf("Aliases[x] = %q, want %q", got, "userId")
	}
}

func TestRelevantAliases(t *testing.T) {
	aliases := map[string]string{
		"x": "userID",
		"y": "orderID",
		"z": "body.Email",
	}

	// Only aliases referenced as tokens in args/field values are kept.
	got := relevantAliases(aliases, []string{"ctx", "x"}, map[string]string{"email": "z"})
	if len(got) != 2 || got["x"] != "userID" || got["z"] != "body.Email" {
		t.Errorf("relevantAliases = %v, want {x:userID z:body.Email}", got)
	}

	// Token match is exact: "xx" must not pick up alias "x".
	if got := relevantAliases(aliases, []string{"xx"}, nil); got != nil {
		t.Errorf("relevantAliases(xx) = %v, want nil (no exact token match)", got)
	}

	// Tokens inside larger expressions are found.
	got = relevantAliases(aliases, []string{"g(x, 1)"}, nil)
	if len(got) != 1 || got["x"] != "userID" {
		t.Errorf("relevantAliases(g(x, 1)) = %v, want {x:userID}", got)
	}

	// No aliases / no matches -> nil.
	if got := relevantAliases(nil, []string{"x"}, nil); got != nil {
		t.Errorf("relevantAliases(nil aliases) = %v, want nil", got)
	}
	if got := relevantAliases(aliases, []string{"amount"}, nil); got != nil {
		t.Errorf("relevantAliases(no match) = %v, want nil", got)
	}

	// Cap: at most maxEdgeAliases entries survive per edge.
	big := map[string]string{}
	var args []string
	for i := 0; i < maxEdgeAliases*2; i++ {
		name := fmt.Sprintf("v%d", i)
		big[name] = "src" + name
		args = append(args, name)
	}
	got = relevantAliases(big, args, nil)
	if len(got) != maxEdgeAliases {
		t.Errorf("relevantAliases cap: got %d entries, want %d", len(got), maxEdgeAliases)
	}
}

func TestRecordAliasCapAndFilters(t *testing.T) {
	var tbl aliasTable
	tbl.record(nil, "x", "x") // self-alias dropped
	tbl.record(nil, "b", "true")
	tbl.record(nil, "n", "nil")
	if got := tbl.at(0); got != nil {
		t.Errorf("self-aliases and literals must be dropped: %v", got)
	}

	for i := 0; i < maxFileAliases+10; i++ {
		tbl.record(nil, fmt.Sprintf("a%d", i), fmt.Sprintf("src%d", i))
	}
	if len(tbl.at(0)) != maxFileAliases {
		t.Errorf("per-file alias cap: got %d, want %d", len(tbl.at(0)), maxFileAliases)
	}
}

// TestAliasesAreScopedPerFunction pins the fix for cross-function alias leaks:
// two functions binding the same local name must not see each other's value.
func TestAliasesAreScopedPerFunction(t *testing.T) {
	src := `package main

func createOrder(req Request) {
	id := req.UserID
	g(id)
}

func shipOrder(order Order) {
	id := order.ID
	h(id)
}
`
	_, edges := parseOrFail(t, "go", "main.go", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g; edges: %+v", edgeNames(edges))
	}
	if got := storage.DecodeEdgeMeta(e.Meta).Aliases["id"]; got != "req.UserID" {
		t.Errorf("g: Aliases[id] = %q, want req.UserID", got)
	}
	e = findEdge(edges, storage.EdgeCall, "h")
	if e == nil {
		t.Fatalf("missing call edge to h; edges: %+v", edgeNames(edges))
	}
	if got := storage.DecodeEdgeMeta(e.Meta).Aliases["id"]; got != "order.ID" {
		t.Errorf("h: Aliases[id] = %q, want order.ID (aliases must not leak across functions)", got)
	}
}

// TestAliasScopingPerLanguage checks the scope boundaries the alias table
// derives per grammar: a name bound in one function must never be visible to a
// call in another.
func TestAliasScopingPerLanguage(t *testing.T) {
	tests := []struct {
		name string
		lang string
		file string
		src  string
	}{
		{
			name: "python", lang: "python", file: "m.py",
			src: `def create(req):
    id = req.user_id
    g(id)

def ship(order):
    id = order.oid
    h(id)
`,
		},
		{
			name: "typescript", lang: "typescript", file: "m.ts",
			src: `function create(req: any) {
	const id = req.userId;
	g(id);
}

function ship(order: any) {
	const id = order.oid;
	h(id);
}
`,
		},
		{
			name: "java", lang: "java", file: "M.java",
			src: `class M {
	void create(Req req) {
		String id = req.userId;
		g(id);
	}

	void ship(Order order) {
		String id = order.oid;
		h(id);
	}
}
`,
		},
		{
			name: "csharp", lang: "csharp", file: "M.cs",
			src: `class M {
	void Create(Req req) {
		var id = req.userId;
		g(id);
	}

	void Ship(Order order) {
		var id = order.oid;
		h(id);
	}
}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, edges := parseOrFail(t, tt.lang, tt.file, tt.src)
			for call, want := range map[string]string{"g": "req.userId", "h": "order.oid"} {
				e := findEdge(edges, storage.EdgeCall, call)
				if e == nil {
					t.Fatalf("missing call edge to %s; edges: %+v", call, edgeNames(edges))
				}
				got := storage.DecodeEdgeMeta(e.Meta).Aliases["id"]
				if tt.lang == "python" {
					want = strings.ReplaceAll(strings.ToLower(want), "userid", "user_id")
				}
				if got != want {
					t.Errorf("%s: Aliases[id] = %q, want %q", call, got, want)
				}
			}
		})
	}
}

// TestRelevantAliasesDeterministicUnderCap pins the ordering fix: the same
// inputs must always yield the same capped subset.
func TestRelevantAliasesDeterministicUnderCap(t *testing.T) {
	aliases := map[string]string{}
	fields := map[string]string{}
	for i := 0; i < maxEdgeAliases*3; i++ {
		name := fmt.Sprintf("v%d", i)
		aliases[name] = "src" + name
		fields[fmt.Sprintf("f%02d", i)] = name
	}
	first := relevantAliases(aliases, nil, fields)
	if len(first) != maxEdgeAliases {
		t.Fatalf("got %d entries, want %d", len(first), maxEdgeAliases)
	}
	for i := 0; i < 20; i++ {
		got := relevantAliases(aliases, nil, fields)
		if len(got) != len(first) {
			t.Fatalf("run %d: size %d, want %d", i, len(got), len(first))
		}
		for k, v := range first {
			if got[k] != v {
				t.Fatalf("run %d: nondeterministic selection, %q missing/differs (%v)", i, k, got)
			}
		}
	}
}

// --- call-result aliases and transitive selection (additions) ---

func TestGoCallEdgeCarriesCallResultAlias(t *testing.T) {
	src := `package main

func f(req Request) {
	x := extractUserID(req)
	g(x)
}
`
	_, edges := parseOrFail(t, "go", "main.go", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g; edges: %+v", edgeNames(edges))
	}
	if got := storage.DecodeEdgeMeta(e.Meta).Aliases["x"]; got != "extractUserID(req)" {
		t.Errorf("Aliases[x] = %q, want %q", got, "extractUserID(req)")
	}
}

func TestGoGetenvAssignmentDoesNotAlias(t *testing.T) {
	src := `package main

import "os"

func f() {
	topic := os.Getenv("ORDERS_TOPIC")
	g(topic)
}
`
	_, edges := parseOrFail(t, "go", "main.go", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g; edges: %+v", edgeNames(edges))
	}
	if got, ok := storage.DecodeEdgeMeta(e.Meta).Aliases["topic"]; ok {
		t.Errorf("Getenv lhs went to consts, must not also be an alias: Aliases[topic] = %q", got)
	}
}

func TestGoCallEdgeCarriesTransitiveAliases(t *testing.T) {
	src := `package main

func f(userID string) {
	y := userID
	x := y
	g(x)
}
`
	_, edges := parseOrFail(t, "go", "main.go", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g; edges: %+v", edgeNames(edges))
	}
	meta := storage.DecodeEdgeMeta(e.Meta)
	if got := meta.Aliases["x"]; got != "y" {
		t.Errorf("Aliases[x] = %q, want %q", got, "y")
	}
	if got := meta.Aliases["y"]; got != "userID" {
		t.Errorf("transitive selection must attach the chained alias: Aliases[y] = %q, want %q (aliases: %v)",
			got, "userID", meta.Aliases)
	}
}

func TestPythonCallEdgeCarriesCallResultAlias(t *testing.T) {
	src := `def f(req):
    x = extract_user_id(req)
    g(x)
`
	_, edges := parseOrFail(t, "python", "main.py", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g; edges: %+v", edgeNames(edges))
	}
	if got := storage.DecodeEdgeMeta(e.Meta).Aliases["x"]; got != "extract_user_id(req)" {
		t.Errorf("Aliases[x] = %q, want %q", got, "extract_user_id(req)")
	}
}

func TestTypeScriptCallEdgeCarriesCallResultAlias(t *testing.T) {
	src := `function f(req: Request) {
	const x = extractUserId(req);
	g(x);
}
`
	_, edges := parseOrFail(t, "typescript", "main.ts", src)

	e := findEdge(edges, storage.EdgeCall, "g")
	if e == nil {
		t.Fatalf("missing call edge to g; edges: %+v", edgeNames(edges))
	}
	if got := storage.DecodeEdgeMeta(e.Meta).Aliases["x"]; got != "extractUserId(req)" {
		t.Errorf("Aliases[x] = %q, want %q", got, "extractUserId(req)")
	}
}

func TestRelevantAliasesOneTransitiveStep(t *testing.T) {
	aliases := map[string]string{
		"x": "y",
		"y": "userID",
		"z": "orderID", // unrelated: not reachable from the args
	}
	got := relevantAliases(aliases, []string{"x"}, nil)
	if len(got) != 2 || got["x"] != "y" || got["y"] != "userID" {
		t.Errorf("relevantAliases = %v, want {x:y y:userID}", got)
	}
}
