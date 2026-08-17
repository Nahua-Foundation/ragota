package ast

import (
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// litResolver resolves plain string literals only.
func litResolver(expr string) (string, bool) { return unquote(expr) }

// litListResolver extracts string literals from "['a', 'b']"-style expressions.
func litListResolver(expr string) []string {
	expr = strings.Trim(strings.TrimSpace(expr), "[]")
	var out []string
	for _, part := range strings.Split(expr, ",") {
		if v, ok := unquote(part); ok {
			out = append(out, v)
		}
	}
	return out
}

func newTestCtx() *fileCtx {
	fc := &fileCtx{path: "test.x", src: nil}
	fc.units = append(fc.units, &storage.ASTUnit{Kind: "function", Name: "caller"})
	return fc
}

func TestHTTPClientRuleMatch(t *testing.T) {
	fc := newTestCtx()
	ds := &detectorSet{HTTP: []httpClientRule{{
		Object:  objectMatch{Contains: []string{"axios"}},
		Methods: map[string]string{"post": "POST"},
		URLArg:  0,
		Conf:    contract.ConfCrossFile,
	}}}
	cs := &callSite{Name: "post", Object: "axios", Args: []string{`'http://svc/api/orders'`, "body"}, Line: 7, Src: 0}

	if !runDetectors(fc, ds, cs, litResolver, nil) {
		t.Fatal("expected rule to match")
	}
	if len(fc.edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(fc.edges))
	}
	e := fc.edges[0]
	if e.Kind != storage.EdgeHTTPCall || e.DstName != "http:POST /api/orders" {
		t.Errorf("edge = %s -> %s", e.Kind, e.DstName)
	}
	if e.Confidence != contract.ConfCrossFile || e.Line != 7 {
		t.Errorf("conf/line = %v/%d", e.Confidence, e.Line)
	}
	m := storage.DecodeEdgeMeta(e.Meta)
	if m.Method != "POST" || m.Path != "/api/orders" || m.Host != "svc" || len(m.Args) != 2 {
		t.Errorf("meta = %+v", m)
	}
}

func TestHTTPClientRuleNoMatch(t *testing.T) {
	rule := httpClientRule{
		Object:  objectMatch{Contains: []string{"axios"}},
		Methods: map[string]string{"post": "POST"},
		URLArg:  0,
		Conf:    contract.ConfCrossFile,
	}
	cases := []struct {
		name string
		cs   callSite
	}{
		{"unknown call name", callSite{Name: "detonate", Object: "axios", Args: []string{`'/x'`}, Src: 0}},
		{"object mismatch", callSite{Name: "post", Object: "db", Args: []string{`'/x'`}, Src: 0}},
		{"unresolvable url", callSite{Name: "post", Object: "axios", Args: []string{"someVar"}, Src: 0}},
		{"no args", callSite{Name: "post", Object: "axios", Src: 0}},
	}
	for _, c := range cases {
		fc := newTestCtx()
		if runDetectors(fc, &detectorSet{HTTP: []httpClientRule{rule}}, &c.cs, litResolver, nil) {
			t.Errorf("%s: expected no match", c.name)
		}
		if len(fc.edges) != 0 {
			t.Errorf("%s: edges emitted on non-match", c.name)
		}
	}
}

func TestHTTPClientRuleURLArgOutOfRange(t *testing.T) {
	fc := newTestCtx()
	ds := &detectorSet{HTTP: []httpClientRule{{
		Methods: map[string]string{"request": "POST"},
		URLArg:  2, // url expected as the third argument
		Conf:    contract.ConfCrossFile,
	}}}
	cs := &callSite{Name: "request", Args: []string{`'a'`, `'b'`}, Src: 0}
	if runDetectors(fc, ds, cs, litResolver, nil) || len(fc.edges) != 0 {
		t.Fatal("rule with URLArg beyond len(args) must not match")
	}
}

func TestHTTPClientRuleMethodArg(t *testing.T) {
	// RestTemplate.exchange(url, HttpMethod.POST, ...) — "ANY" resolved from arg 1.
	fc := newTestCtx()
	ds := &detectorSet{HTTP: []httpClientRule{{
		Methods:   map[string]string{"exchange": "ANY"},
		URLArg:    0,
		MethodArg: 1,
		Conf:      contract.ConfCrossFile,
	}}}
	cs := &callSite{Name: "exchange", Args: []string{`"http://b/api/pay"`, "HttpMethod.POST", "req"}, Src: 0}
	if !runDetectors(fc, ds, cs, litResolver, nil) {
		t.Fatal("expected match")
	}
	if fc.edges[0].DstName != "http:POST /api/pay" {
		t.Errorf("dst = %s", fc.edges[0].DstName)
	}
}

func TestHTTPClientRuleMethodFromField(t *testing.T) {
	// fetch(url, {method: 'put'}) — method overridden from the options object.
	ds := &detectorSet{HTTP: []httpClientRule{{
		Methods:         map[string]string{"fetch": "GET"},
		URLArg:          0,
		MethodFromField: "method",
		Conf:            contract.ConfCrossFile,
	}}}

	fc := newTestCtx()
	cs := &callSite{Name: "fetch", Args: []string{`'/api/x'`, "opts"},
		Fields: map[string]string{"method": `'put'`}, Src: 0}
	if !runDetectors(fc, ds, cs, litResolver, nil) || fc.edges[0].DstName != "http:PUT /api/x" {
		t.Fatalf("dst = %v", edgeNames(fc.edges))
	}

	// Without the field the table default applies.
	fc = newTestCtx()
	cs = &callSite{Name: "fetch", Args: []string{`'/api/x'`}, Src: 0}
	if !runDetectors(fc, ds, cs, litResolver, nil) || fc.edges[0].DstName != "http:GET /api/x" {
		t.Fatalf("dst = %v", edgeNames(fc.edges))
	}
}

func TestKafkaProduceRuleTopicArg(t *testing.T) {
	fc := newTestCtx()
	ds := &detectorSet{Produce: []kafkaProduceRule{{
		Methods: []string{"send"}, TopicArg: 0, Conf: contract.ConfHigh,
	}}}
	cs := &callSite{Name: "send", Object: "producer", Args: []string{`"orders.created"`, "payload"}, Src: 0}
	if !runDetectors(fc, ds, cs, litResolver, nil) {
		t.Fatal("expected match")
	}
	e := fc.edges[0]
	if e.Kind != storage.EdgeProduces || e.DstName != "topic:orders.created" || e.Confidence != contract.ConfHigh {
		t.Errorf("edge = %+v", e)
	}
	if m := storage.DecodeEdgeMeta(e.Meta); m.Topic != "orders.created" || len(m.Args) != 2 {
		t.Errorf("meta = %+v", m)
	}
}

func TestKafkaProduceRuleTopicFromFieldOmitArgs(t *testing.T) {
	// kafkajs producer.send({topic: 'x', messages: []}) — topic from the object,
	// args omitted from the meta.
	fc := newTestCtx()
	ds := &detectorSet{Produce: []kafkaProduceRule{{
		Methods: []string{"send"}, TopicFromField: "topic", OmitArgs: true, Conf: contract.ConfHigh,
	}}}
	fields := map[string]string{"topic": `'checkout.started'`, "messages": "[]"}
	cs := &callSite{Name: "send", Args: []string{"{...}"}, Fields: fields, Src: 0}
	if !runDetectors(fc, ds, cs, litResolver, nil) {
		t.Fatal("expected match")
	}
	m := storage.DecodeEdgeMeta(fc.edges[0].Meta)
	if m.Topic != "checkout.started" || m.Args != nil || m.Fields["messages"] != "[]" {
		t.Errorf("meta = %+v", m)
	}

	// No topic field -> no match, no edge.
	fc = newTestCtx()
	cs = &callSite{Name: "send", Args: []string{"x"}, Fields: map[string]string{"messages": "[]"}, Src: 0}
	if runDetectors(fc, ds, cs, litResolver, nil) || len(fc.edges) != 0 {
		t.Fatal("send without topic field must not match")
	}
}

func TestKafkaConsumeRuleTopicsFromFieldsList(t *testing.T) {
	ds := &detectorSet{Consume: []kafkaConsumeRule{{
		Methods:          []string{"subscribe"},
		TopicsFromFields: []string{"topic", "topics"},
		Conf:             contract.ConfHigh,
	}}}

	// topics list -> one edge per topic
	fc := newTestCtx()
	cs := &callSite{Name: "subscribe", Args: []string{"{...}"},
		Fields: map[string]string{"topics": `['a.events', 'b.events']`}, Src: 0}
	if !runDetectors(fc, ds, cs, litResolver, litListResolver) {
		t.Fatal("expected match")
	}
	if len(fc.edges) != 2 || fc.edges[0].DstName != "topic:a.events" || fc.edges[1].DstName != "topic:b.events" {
		t.Fatalf("edges = %v", edgeNames(fc.edges))
	}

	// "topic" takes precedence over "topics"
	fc = newTestCtx()
	cs = &callSite{Name: "subscribe", Args: []string{"{...}"},
		Fields: map[string]string{"topic": `'x'`, "topics": `['y']`}, Src: 0}
	if !runDetectors(fc, ds, cs, litResolver, litListResolver) {
		t.Fatal("expected match")
	}
	if len(fc.edges) != 1 || fc.edges[0].DstName != "topic:x" {
		t.Fatalf("edges = %v", edgeNames(fc.edges))
	}
}

func TestKafkaConsumeRuleTopicArgList(t *testing.T) {
	ds := &detectorSet{Consume: []kafkaConsumeRule{{
		Methods: []string{"subscribe"}, TopicArg: 0, TopicArgList: true, Conf: contract.ConfHigh,
	}}}

	fc := newTestCtx()
	cs := &callSite{Name: "subscribe", Args: []string{`["orders.created", "orders.paid"]`}, Src: 0}
	if !runDetectors(fc, ds, cs, litResolver, litListResolver) {
		t.Fatal("expected match")
	}
	if len(fc.edges) != 2 {
		t.Fatalf("edges = %v", edgeNames(fc.edges))
	}

	// A present but unresolvable list arg still counts as a match (suppresses
	// the generic call fallback) while emitting nothing.
	fc = newTestCtx()
	cs = &callSite{Name: "subscribe", Args: []string{"dynamicTopics"}, Src: 0}
	if !runDetectors(fc, ds, cs, litResolver, litListResolver) {
		t.Fatal("list-mode rule must match on a present arg")
	}
	if len(fc.edges) != 0 {
		t.Fatalf("edges = %v, want none", edgeNames(fc.edges))
	}
}

func TestRulePriorityFirstMatchWins(t *testing.T) {
	fc := newTestCtx()
	ds := &detectorSet{HTTP: []httpClientRule{
		{Methods: map[string]string{"get": "GET"}, URLArg: 0, Conf: contract.ConfHigh},
		{Methods: map[string]string{"get": "DELETE"}, URLArg: 0, Conf: contract.ConfWeak},
	}}
	cs := &callSite{Name: "get", Object: "c", Args: []string{`'/x'`}, Src: 0}
	if !runDetectors(fc, ds, cs, litResolver, nil) {
		t.Fatal("expected match")
	}
	if len(fc.edges) != 1 || fc.edges[0].DstName != "http:GET /x" || fc.edges[0].Confidence != contract.ConfHigh {
		t.Fatalf("first rule must win; edges = %v", edgeNames(fc.edges))
	}
}

func TestNegativeSrcEmitsNoEdgeButMatches(t *testing.T) {
	// A matched call outside any unit is swallowed (addEdge drops it) but
	// still counts as handled, mirroring the hand-written extractors.
	fc := newTestCtx()
	ds := &detectorSet{HTTP: []httpClientRule{{
		Methods: map[string]string{"get": "GET"}, URLArg: 0, Conf: contract.ConfHigh,
	}}}
	cs := &callSite{Name: "get", Args: []string{`'/x'`}, Src: -1}
	if !runDetectors(fc, ds, cs, litResolver, nil) {
		t.Fatal("expected match")
	}
	if len(fc.edges) != 0 {
		t.Fatalf("edges = %v, want none", edgeNames(fc.edges))
	}
}

func TestObjectMatchSemantics(t *testing.T) {
	cases := []struct {
		m      objectMatch
		object string
		want   bool
	}{
		{objectMatch{}, "anything", true}, // zero value matches all
		{objectMatch{Exact: []string{"http"}}, "http", true},
		{objectMatch{Exact: []string{"http"}}, "myhttp", false},
		{objectMatch{Suffix: []string{"Client"}}, "orderClient", true},
		{objectMatch{Suffix: []string{"Client"}}, "s.client", false}, // case-sensitive
		{objectMatch{Contains: []string{"kafka"}, Fold: true}, "KafkaTemplate", true},
		{objectMatch{Contains: []string{"kafka"}}, "KafkaTemplate", false}, // no fold
	}
	for i, c := range cases {
		if got := c.m.matches(c.object); got != c.want {
			t.Errorf("case %d: matches(%q) = %v, want %v", i, c.object, got, c.want)
		}
	}
}

// TestNewFrameworkAsDataRow is the point of the engine: supporting a new
// client framework is appending one rule to a language's detectorSet.
func TestNewFrameworkAsDataRow(t *testing.T) {
	cs := &callSite{Name: "retrieve", Object: "webClient", Args: []string{`"http://users/api/users/1"`}, Line: 3, Src: 0}

	// Before: an unknown framework call does not match.
	fc := newTestCtx()
	base := detectorSet{HTTP: append([]httpClientRule(nil), javaDetectors.HTTP...)}
	if runDetectors(fc, &base, cs, litResolver, nil) {
		t.Fatal("retrieve must not match the stock rules")
	}

	// After: one data row adds Spring WebClient-style support.
	base.HTTP = append(base.HTTP, httpClientRule{
		Object:  objectMatch{Contains: []string{"webclient"}, Fold: true},
		Methods: map[string]string{"retrieve": "GET"},
		URLArg:  0,
		Conf:    contract.ConfCrossFile,
	})
	if !runDetectors(fc, &base, cs, litResolver, nil) {
		t.Fatal("expected the new rule to match")
	}
	e := fc.edges[0]
	if e.Kind != storage.EdgeHTTPCall || e.DstName != "http:GET /api/users/1" {
		t.Errorf("edge = %s -> %s", e.Kind, e.DstName)
	}
	if m := storage.DecodeEdgeMeta(e.Meta); m.Host != "users" {
		t.Errorf("meta = %+v", m)
	}
}

// --- precision gates --------------------------------------------------------
//
// The three heuristics below fired on code that had nothing to do with HTTP or
// gRPC, measured across 13 open-source repositories. Each test pins both
// directions: the phantom shape stays out, the real one still lands.

// TestJavaPutDeleteNeedsHTTPClientReceiver: RestTemplate.put/delete share their
// names with Map and Collection members. Unconstrained, the rule turned every
// props.put(...) in Kafka, Elasticsearch and Conductor into an http_call.
func TestJavaPutDeleteNeedsHTTPClientReceiver(t *testing.T) {
	tests := []struct {
		name string
		body string
		decl string
		want string // expected http_call key, "" for none
	}{
		{
			name: "map put is not an http call",
			body: `props.put("key.deserializer", StringDeserializer.class);`,
		},
		{
			name: "map put with a numeric value is not an http call",
			body: `map.put("test2", 2);`,
		},
		{
			name: "declared map receiver named like a client is not an http call",
			decl: `private final Map<String, String> httpClientProps = new HashMap<>();`,
			body: `httpClientProps.put("bootstrap.servers", "LOCALHOST:9092");`,
		},
		{
			name: "collection remove-style delete is not an http call",
			body: `cache.delete("orders-cache-key");`,
		},
		{
			name: "rest template put is an http call",
			body: `restTemplate.put("http://orders/api/orders/1", body);`,
			want: "http:PUT /api/orders/1",
		},
		{
			name: "rest template delete is an http call",
			body: `restTemplate.delete("http://orders/api/orders/1");`,
			want: "http:DELETE /api/orders/1",
		},
		{
			name: "receiver typed as a rest template is an http call",
			decl: `private final RestTemplate client;`,
			body: `client.put("http://orders/api/orders/1", body);`,
			want: "http:PUT /api/orders/1",
		},
		{
			name: "get-style methods keep working on any receiver",
			body: `template.getForObject("http://orders/api/orders", Order.class);`,
			want: "http:GET /api/orders",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "package com.acme;\n\nclass Svc {\n    " + tt.decl +
				"\n    void go(Object body) {\n        " + tt.body + "\n    }\n}\n"
			_, edges := parseOrFail(t, "java", "Svc.java", src)

			if tt.want == "" {
				for _, e := range edges {
					if e.Kind == storage.EdgeHTTPCall {
						t.Fatalf("unexpected http_call %q; edges: %+v", e.DstName, edgeNames(edges))
					}
				}
				return
			}
			if findEdge(edges, storage.EdgeHTTPCall, tt.want) == nil {
				t.Fatalf("missing http_call %q; edges: %+v", tt.want, edgeNames(edges))
			}
		})
	}
}

// TestURLShapeGate pins the shape an http_call target must have. The gate is
// about shape, not segment count: "/health" is a route, "test2" is not.
func TestURLShapeGate(t *testing.T) {
	tests := []struct {
		lit  string
		want bool
	}{
		{"/health", true}, // single segment, still a route
		{"/", true},
		{"/api/orders/1", true},
		{"http://orders/api/orders", true},
		{"https://orders", true},
		{"grpc://orders/Svc", true},
		{"key.deserializer", false},
		{"test2", false},
		{"LOCALHOST:9092", false},
		{"--BOOTSTRAP-SERVER", false},
		{"", false},
		{"org.apache.kafka.common.serialization.StringSerializer", false},
		{"SELECT * FROM orders", false}, // whitespace is never a request target
		{"api/orders", false},           // relative: not a resolvable route key
	}
	for _, tt := range tests {
		if got := isURLShaped(tt.lit); got != tt.want {
			t.Errorf("isURLShaped(%q) = %v, want %v", tt.lit, got, tt.want)
		}
	}
}

// TestURLShapeGateAppliesToRules checks the gate where it matters: a rule with
// no receiver constraint still cannot key a route on a non-URL literal, while
// an interpolated base URL keeps resolving through the template path.
func TestURLShapeGateAppliesToRules(t *testing.T) {
	ds := &detectorSet{HTTP: []httpClientRule{{
		Methods: map[string]string{"put": "PUT"}, URLArg: 0, Conf: contract.ConfCrossFile,
	}}}
	tests := []struct {
		arg  string
		want string // expected edge key, "" for no edge
	}{
		{`"key.deserializer"`, ""},
		{`"test2"`, ""},
		{`"META-INF/services/foo"`, ""}, // resolved literal: nothing left to template
		{`"/health"`, "http:PUT /health"},
		{`"http://orders/api/orders"`, "http:PUT /api/orders"},
		{`"${orders.url}/api/orders"`, "http:PUT /api/orders"},
		{`"%s/api/orders"`, "http:PUT /api/orders"},
	}
	for _, tt := range tests {
		fc := newTestCtx()
		cs := &callSite{Name: "put", Object: "c", Args: []string{tt.arg, "body"}, Line: 1, Src: 0}
		matched := runDetectors(fc, ds, cs, litResolver, nil)
		if tt.want == "" {
			if matched || len(fc.edges) != 0 {
				t.Errorf("arg %s: unexpected edges %v", tt.arg, edgeNames(fc.edges))
			}
			continue
		}
		if !matched || len(fc.edges) != 1 {
			t.Fatalf("arg %s: edges = %v, want %q", tt.arg, edgeNames(fc.edges), tt.want)
		}
		if got := fc.edges[0].DstName; got != tt.want {
			t.Errorf("arg %s: edge = %q, want %q", tt.arg, got, tt.want)
		}
	}
}

// TestGRPCStubTypeNeedsEvidence: a receiver whose declared type ends in Client
// is the naming convention of every HTTP and infrastructure SDK, so the type
// name alone must not produce an rpc_call.
func TestGRPCStubTypeNeedsEvidence(t *testing.T) {
	tests := []struct {
		name string
		lang string
		file string
		src  string
		want string // expected rpc_call key, "" for none
	}{
		{
			name: "java sdk client without gRPC anywhere", lang: "java", file: "Svc.java",
			src: `package com.acme;

class Svc {
    private final IndicesAdminClient client;

    void go(Request req) {
        client.doThing(req);
    }
}
`,
		},
		{
			name: "java admin client without gRPC anywhere", lang: "java", file: "Admin.java",
			src: `package com.acme;

class Admin {
    private final NodeClient node;

    void go(Request req) {
        node.execute(req);
    }
}
`,
		},
		{
			name: "java client corroborated by a gRPC import", lang: "java", file: "Gateway.java",
			src: `package com.acme;

import io.grpc.ManagedChannel;

class Gateway {
    private final OrdersClient orders;

    void go(Request req) {
        orders.doThing(req);
    }
}
`,
			want: "grpc:Orders/DoThing",
		},
		{
			name: "java generated stub shape needs no import", lang: "java", file: "Stubs.java",
			src: `package com.acme;

class Stubs {
    private final OrderServiceGrpc.OrderServiceBlockingStub orders;

    void go(CreateOrderRequest req) {
        orders.createOrder(req);
    }
}
`,
			want: "grpc:OrderService/CreateOrder",
		},
		{
			name: "typescript sdk client without gRPC anywhere", lang: "typescript", file: "svc.ts",
			src: `export class Svc {
  constructor(private search: SearchIndexClient) {}

  go(req: any) {
    return this.search.doThing(req);
  }
}
`,
		},
		{
			name: "go qualified generated client", lang: "go", file: "handler.go",
			src: `package handler

type Handler struct {
	orders pb.OrderServiceClient
}

func (h *Handler) Create(ctx context.Context) {
	h.orders.CreateOrder(ctx, nil)
}
`,
			want: "grpc:OrderService/CreateOrder",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, edges := parseOrFail(t, tt.lang, tt.file, tt.src)
			if tt.want == "" {
				for _, e := range edges {
					if e.Kind == storage.EdgeRPCCall {
						t.Fatalf("unexpected rpc_call %q; edges: %+v", e.DstName, edgeNames(edges))
					}
				}
				return
			}
			if findEdge(edges, storage.EdgeRPCCall, tt.want) == nil {
				t.Fatalf("missing rpc_call %q; edges: %+v", tt.want, edgeNames(edges))
			}
		})
	}
}

func TestIsGeneratedStubType(t *testing.T) {
	tests := []struct {
		typ  string
		want bool
	}{
		{"OrderServiceGrpc.OrderServiceBlockingStub", true},
		{"OrderServiceStub", true},
		{"pb.OrderServiceClient", true},  // qualified: generated
		{"*pb.OrderServiceClient", true}, // go pointer
		{"OrderService.OrderServiceClient", true},
		{"OrderServiceClient", true}, // proto service naming
		{"IndicesAdminClient", false},
		{"NodeClient", false},
		{"ElasticClient", false},
		{"TransportClient", false},
		{"OrderRepository", false},
	}
	for _, tt := range tests {
		if got := isGeneratedStubType(tt.typ); got != tt.want {
			t.Errorf("isGeneratedStubType(%q) = %v, want %v", tt.typ, got, tt.want)
		}
	}
}

func TestHasGRPCImport(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{"java runtime", "import io.grpc.ManagedChannel;", true},
		{"go runtime", `import "google.golang.org/grpc"`, true},
		{"typescript runtime", `import { credentials } from '@grpc/grpc-js';`, true},
		{"python runtime", "import grpc\n", true},
		{"csharp runtime", "using Grpc.Core;", true},
		{"generated python stub", "from orders_pb2_grpc import OrderServiceStub", true},
		{"java factory call", "var s = OrderServiceGrpc.newBlockingStub(channel);", true},
		{"plain spring file", "import org.springframework.web.client.RestTemplate;", false},
		{"word in prose", "// this service does not speak RPC at all", false},
	}
	for _, tt := range tests {
		if got := hasGRPCImport([]byte(tt.src)); got != tt.want {
			t.Errorf("%s: hasGRPCImport = %v, want %v", tt.name, got, tt.want)
		}
	}
}
