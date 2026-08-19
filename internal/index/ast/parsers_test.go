package ast

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

func parseOrFail(t *testing.T, lang, path, src string) ([]*domain.ASTUnit, []*domain.Edge) {
	t.Helper()
	p := GetParserForLanguage(lang)
	units, edges, err := p.Parse(path, src)
	if err != nil {
		t.Fatalf("Parse(%s): %v", lang, err)
	}
	return units, edges
}

func findUnit(units []*domain.ASTUnit, kind, name string) *domain.ASTUnit {
	for _, u := range units {
		if u.Kind == kind && u.Name == name {
			return u
		}
	}
	return nil
}

func findEdge(edges []*domain.Edge, kind, dstName string) *domain.Edge {
	for _, e := range edges {
		if e.Kind == kind && e.DstName == dstName {
			return e
		}
	}
	return nil
}

func TestGoExtractorRoutesAndCalls(t *testing.T) {
	src := `package main

import "net/http"

func main() {
	http.HandleFunc("POST /api/v1/orders", CreateOrderHandler)
	r.Get("/health", healthHandler)
}

func CreateOrderHandler(w http.ResponseWriter, r *http.Request) {
	validate(r)
}

func validate(r *http.Request) {}
`
	units, edges := parseOrFail(t, "go", "main.go", src)

	if u := findUnit(units, store.KindHTTPRoute, "POST /api/v1/orders"); u == nil {
		t.Errorf("missing POST route unit; units: %+v", names(units))
	} else if u.Qualified != "http:POST /api/v1/orders" {
		t.Errorf("route qualified = %q", u.Qualified)
	}
	if findUnit(units, store.KindHTTPRoute, "GET /health") == nil {
		t.Errorf("missing GET route unit")
	}
	if e := findEdge(edges, store.EdgeHandledBy, "CreateOrderHandler"); e == nil {
		t.Errorf("missing handled_by edge; edges: %+v", edgeNames(edges))
	}
	if e := findEdge(edges, store.EdgeCall, "validate"); e == nil {
		t.Errorf("missing call edge to validate")
	}
}

func TestGoExtractorGRPCClientAndServer(t *testing.T) {
	src := `package main

func callOrders(userID string) {
	client := pb.NewOrderServiceClient(conn)
	client.CreateOrder(ctx, &pb.CreateOrderRequest{UserId: userID, Amount: amt})
}

func register(s *grpc.Server) {
	pb.RegisterOrderServiceServer(s, &server{})
}

func (s *server) CreateOrder(ctx context.Context, req *pb.CreateOrderRequest) (*pb.CreateOrderResponse, error) {
	return nil, nil
}
`
	units, edges := parseOrFail(t, "go", "grpc.go", src)

	e := findEdge(edges, store.EdgeRPCCall, "grpc:OrderService/CreateOrder")
	if e == nil {
		t.Fatalf("missing rpc_call edge; edges: %+v", edgeNames(edges))
	}
	meta := store.DecodeEdgeMeta(e.Meta)
	if meta.Fields["UserId"] != "userID" {
		t.Errorf("rpc_call meta fields = %+v, want UserId->userID", meta.Fields)
	}
	if findEdge(edges, store.EdgeImplementsRPC, "grpc:OrderService/CreateOrder") == nil {
		t.Errorf("missing implements_rpc edge; edges: %+v", edgeNames(edges))
	}
	_ = units
}

func TestGoExtractorKafka(t *testing.T) {
	src := `package main

const ordersTopic = "orders.created"

func publish(userID string) {
	w := &kafka.Writer{Topic: ordersTopic}
	w.WriteMessages(ctx, kafka.Message{Value: b})
}

func consume() {
	r := kafka.NewReader(kafka.ReaderConfig{Topic: "orders.created"})
	r.ReadMessage(ctx)
}
`
	_, edges := parseOrFail(t, "go", "kafka.go", src)

	if findEdge(edges, store.EdgeProduces, "topic:orders.created") == nil {
		t.Errorf("missing produces edge; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeConsumes, "topic:orders.created") == nil {
		t.Errorf("missing consumes edge; edges: %+v", edgeNames(edges))
	}
}

func TestJavaExtractor(t *testing.T) {
	src := `package com.acme.billing;

@RestController
@RequestMapping("/api/billing")
public class BillingController {

    @PostMapping("/charge")
    public ChargeResponse charge(@RequestBody ChargeRequest req) {
        return process(req);
    }

    @KafkaListener(topics = "orders.created")
    public void onOrderCreated(String message) {
        restTemplate.postForObject("http://notifier/api/notify/send", payload, Void.class);
    }
}
`
	units, edges := parseOrFail(t, "java", "BillingController.java", src)

	if findUnit(units, "class", "BillingController") == nil {
		t.Fatalf("missing class unit; units: %+v", names(units))
	}
	if u := findUnit(units, store.KindHTTPRoute, "POST /api/billing/charge"); u == nil {
		t.Errorf("missing route unit; units: %+v", names(units))
	}
	if findEdge(edges, store.EdgeConsumes, "topic:orders.created") == nil {
		t.Errorf("missing consumes edge; edges: %+v", edgeNames(edges))
	}
	e := findEdge(edges, store.EdgeHTTPCall, "http:POST /api/notify/send")
	if e == nil {
		t.Errorf("missing http_call edge; edges: %+v", edgeNames(edges))
	}
	m := findUnit(units, "method", "onOrderCreated")
	if m == nil || m.Qualified != "com.acme.billing.BillingController.onOrderCreated" {
		t.Errorf("bad qualified for onOrderCreated: %+v", m)
	}
}

func TestCSharpExtractor(t *testing.T) {
	src := `namespace Acme.Notifier;

[ApiController]
[Route("api/[controller]")]
public class NotifyController : ControllerBase
{
    [HttpPost("send")]
    public IActionResult Send([FromBody] NotifyRequest req)
    {
        Deliver(req);
        return Ok();
    }

    private void Deliver(NotifyRequest req)
    {
        httpClient.PostAsync("http://audit/api/audit", content);
        consumer.Subscribe("notify.requested");
    }
}
`
	units, edges := parseOrFail(t, "csharp", "NotifyController.cs", src)

	if findUnit(units, "class", "NotifyController") == nil {
		t.Fatalf("missing class unit; units: %+v", names(units))
	}
	if findUnit(units, store.KindHTTPRoute, "POST /api/notify/send") == nil {
		t.Errorf("missing route unit; units: %+v", names(units))
	}
	if findEdge(edges, store.EdgeCall, "Deliver") == nil {
		t.Errorf("missing call edge; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeHTTPCall, "http:POST /api/audit") == nil {
		t.Errorf("missing http_call edge; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeConsumes, "topic:notify.requested") == nil {
		t.Errorf("missing consumes edge; edges: %+v", edgeNames(edges))
	}
}

func TestTypeScriptExtractor(t *testing.T) {
	src := `const GATEWAY = 'http://gateway/api/v1/orders';

export async function createOrder(userId: string, amount: number) {
  await axios.post(GATEWAY, { user_id: userId, amount });
}

app.post('/checkout', checkoutHandler);

async function checkoutHandler(req, res) {
  await createOrder(req.body.user_id, req.body.amount);
}

async function startConsumer() {
  await consumer.subscribe({ topic: 'payments.completed' });
  await producer.send({ topic: 'checkout.started', messages: [] });
}
`
	units, edges := parseOrFail(t, "typescript", "web.ts", src)

	if findUnit(units, "function", "createOrder") == nil {
		t.Fatalf("missing function unit; units: %+v", names(units))
	}
	e := findEdge(edges, store.EdgeHTTPCall, "http:POST /api/v1/orders")
	if e == nil {
		t.Fatalf("missing http_call edge; edges: %+v", edgeNames(edges))
	}
	meta := store.DecodeEdgeMeta(e.Meta)
	if meta.Fields["user_id"] != "userId" {
		t.Errorf("http_call fields = %+v, want user_id->userId", meta.Fields)
	}
	if findUnit(units, store.KindHTTPRoute, "POST /checkout") == nil {
		t.Errorf("missing route unit; units: %+v", names(units))
	}
	if findEdge(edges, store.EdgeConsumes, "topic:payments.completed") == nil {
		t.Errorf("missing consumes edge; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeProduces, "topic:checkout.started") == nil {
		t.Errorf("missing produces edge; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeCall, "createOrder") == nil {
		t.Errorf("missing call edge; edges: %+v", edgeNames(edges))
	}
}

func TestProtoParser(t *testing.T) {
	src := `syntax = "proto3";

package orders;

service OrderService {
  rpc CreateOrder (CreateOrderRequest) returns (CreateOrderResponse);
}

message CreateOrderRequest {
  string user_id = 1;
  double amount = 2;
}

message CreateOrderResponse {
  string order_id = 1;
}
`
	units, edges := parseOrFail(t, "proto", "orders.proto", src)

	if u := findUnit(units, store.KindRPCMethod, "CreateOrder"); u == nil {
		t.Fatalf("missing rpc unit; units: %+v", names(units))
	} else if u.Qualified != "grpc:orders.OrderService/CreateOrder" {
		t.Errorf("rpc qualified = %q", u.Qualified)
	}
	if u := findUnit(units, store.KindProtoField, "user_id"); u == nil {
		t.Errorf("missing proto_field unit; units: %+v", names(units))
	} else if u.Qualified != "proto:orders.CreateOrderRequest.user_id" {
		t.Errorf("field qualified = %q", u.Qualified)
	}
	if findEdge(edges, store.EdgeRPCRequest, "proto:orders.CreateOrderRequest") == nil {
		t.Errorf("missing rpc_request edge; edges: %+v", edgeNames(edges))
	}
}

func TestParamNames(t *testing.T) {
	cases := []struct {
		lang, sig string
		want      []string
	}{
		{"go", "(ctx context.Context, req *pb.CreateOrderRequest)", []string{"ctx", "req"}},
		{"java", "(@RequestBody ChargeRequest req)", []string{"req"}},
		{"csharp", "([FromBody] NotifyRequest req)", []string{"req"}},
		{"typescript", "(userId: string, amount: number)", []string{"userId", "amount"}},
		// Generics must not be split on the inner comma.
		{"java", "(Map<String, Integer> m, int x)", []string{"m", "x"}},
		// Go variadic.
		{"go", "(args ...string)", []string{"args"}},
		// TS rest and default parameters.
		{"typescript", "(...args: string[])", []string{"args"}},
		{"typescript", "(amount: number = 0)", []string{"amount"}},
		// Java/C# varargs and C# default value.
		{"java", "(String... names)", []string{"names"}},
		{"csharp", "(int x = 5)", []string{"x"}},
	}
	for _, c := range cases {
		got := contract.ParamNames(c.lang, c.sig)
		if len(got) != len(c.want) {
			t.Errorf("contract.ParamNames(%s, %q) = %v, want %v", c.lang, c.sig, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("contract.ParamNames(%s, %q) = %v, want %v", c.lang, c.sig, got, c.want)
				break
			}
		}
	}
}

func TestSplitTopLevel(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		// Go signature: plain top-level split.
		{"ctx context.Context, req *pb.CreateOrderRequest",
			[]string{"ctx context.Context", " req *pb.CreateOrderRequest"}},
		// Java generics: the comma inside Map<...> is nested, not a separator.
		{"Map<String, Integer> m, int x",
			[]string{"Map<String, Integer> m", " int x"}},
		{"List<List<Integer>> xs, int y",
			[]string{"List<List<Integer>> xs", " int y"}},
		// Comparison: '<' preceded by a space is not a generic and must not
		// swallow the following separator.
		{"a < b, c", []string{"a < b", " c"}},
		{"i <= n, j", []string{"i <= n", " j"}},
		// Unbalanced '>' (arrow, comparison) must not push depth negative and
		// hide nesting that follows.
		{"x -> y, z", []string{"x -> y", " z"}},
		{"a > b, f(c, d), e", []string{"a > b", " f(c, d)", " e"}},
		// Regular brackets still nest.
		{"f(a, b), c", []string{"f(a, b)", " c"}},
	}
	for _, c := range cases {
		got := contract.SplitTopLevel(c.in, ',')
		if len(got) != len(c.want) {
			t.Errorf("contract.SplitTopLevel(%q) = %q, want %q", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("contract.SplitTopLevel(%q) = %q, want %q", c.in, got, c.want)
				break
			}
		}
	}
}

func names(units []*domain.ASTUnit) []string {
	var out []string
	for _, u := range units {
		out = append(out, u.Kind+":"+u.Name)
	}
	return out
}

func edgeNames(edges []*domain.Edge) []string {
	var out []string
	for _, e := range edges {
		out = append(out, e.Kind+"->"+e.DstName)
	}
	return out
}
