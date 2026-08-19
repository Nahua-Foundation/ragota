package ast

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/store"
)

// Coverage tests for the client-side detections added on top of the base
// extractors: gRPC client stubs in Java/C#/TS, fluent HTTP clients
// (WebClient chains, RestSharp requests, ky/got/superagent) and the Kotlin
// extractor.

func TestJavaGRPCClientStub(t *testing.T) {
	src := `package com.acme.gateway;

public class OrderClient {
    void call(CreateOrderRequest req) {
        OrderServiceGrpc.OrderServiceBlockingStub stub = OrderServiceGrpc.newBlockingStub(channel);
        stub.createOrder(req);
    }
}
`
	_, edges := parseOrFail(t, "java", "OrderClient.java", src)

	// lowerCamel stub method is capitalized to match the proto method name.
	e := findEdge(edges, store.EdgeRPCCall, "grpc:OrderService/CreateOrder")
	if e == nil {
		t.Fatalf("missing rpc_call edge; edges: %+v", edgeNames(edges))
	}
	meta := store.DecodeEdgeMeta(e.Meta)
	if len(meta.Args) != 1 || meta.Args[0] != "req" {
		t.Errorf("rpc_call args = %+v, want [req]", meta.Args)
	}
}

func TestCSharpGRPCClient(t *testing.T) {
	src := `namespace Acme.Gateway;

public class OrderCaller
{
    public async Task CallAsync(CreateOrderRequest req)
    {
        var client = new OrderService.OrderServiceClient(channel);
        await client.CreateOrderAsync(req);
    }
}
`
	_, edges := parseOrFail(t, "csharp", "OrderCaller.cs", src)

	// The Async suffix is stripped from the stub method name.
	if findEdge(edges, store.EdgeRPCCall, "grpc:OrderService/CreateOrder") == nil {
		t.Fatalf("missing rpc_call edge; edges: %+v", edgeNames(edges))
	}
}

func TestTypeScriptGRPCClient(t *testing.T) {
	src := `const client = new OrderServiceClient(addr, credentials);

export function createOrder(userId: string) {
  client.createOrder({ user_id: userId }, (err, res) => {});
}
`
	_, edges := parseOrFail(t, "typescript", "orders.ts", src)

	e := findEdge(edges, store.EdgeRPCCall, "grpc:OrderService/CreateOrder")
	if e == nil {
		t.Fatalf("missing rpc_call edge; edges: %+v", edgeNames(edges))
	}
	meta := store.DecodeEdgeMeta(e.Meta)
	if meta.Fields["user_id"] != "userId" {
		t.Errorf("rpc_call fields = %+v, want user_id->userId", meta.Fields)
	}
}

func TestJavaWebClientChain(t *testing.T) {
	src := `package com.acme.billing;

public class Notifier {
    void notifyUser(String userId) {
        webClient.post().uri("/api/notify/send").bodyValue(body).retrieve();
    }
}
`
	_, edges := parseOrFail(t, "java", "Notifier.java", src)

	e := findEdge(edges, store.EdgeHTTPCall, "http:POST /api/notify/send")
	if e == nil {
		t.Fatalf("missing http_call edge; edges: %+v", edgeNames(edges))
	}
	meta := store.DecodeEdgeMeta(e.Meta)
	if meta.Method != "POST" || meta.Path != "/api/notify/send" {
		t.Errorf("http_call meta = %+v", meta)
	}
}

func TestTypeScriptFluentHTTPClients(t *testing.T) {
	src := `const AUDIT = 'http://audit/api/audit/log';

async function report() {
  await got.post(AUDIT, { json: { ok: true } });
  await ky.post('/api/metrics');
  await superagent.get('http://users/api/users');
}
`
	_, edges := parseOrFail(t, "typescript", "report.ts", src)

	if findEdge(edges, store.EdgeHTTPCall, "http:POST /api/audit/log") == nil {
		t.Errorf("missing got http_call edge; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeHTTPCall, "http:POST /api/metrics") == nil {
		t.Errorf("missing ky http_call edge; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeHTTPCall, "http:GET /api/users") == nil {
		t.Errorf("missing superagent http_call edge; edges: %+v", edgeNames(edges))
	}
}

func TestCSharpRestRequest(t *testing.T) {
	src := `namespace Acme.Audit;

public class AuditClient
{
    public void Send()
    {
        var request = new RestRequest("/api/audit/log", Method.Post);
        var ping = new RestRequest("/api/health");
    }
}
`
	_, edges := parseOrFail(t, "csharp", "AuditClient.cs", src)

	if findEdge(edges, store.EdgeHTTPCall, "http:POST /api/audit/log") == nil {
		t.Errorf("missing POST http_call edge; edges: %+v", edgeNames(edges))
	}
	// Without a Method argument RestSharp defaults to GET.
	if findEdge(edges, store.EdgeHTTPCall, "http:GET /api/health") == nil {
		t.Errorf("missing GET http_call edge; edges: %+v", edgeNames(edges))
	}
}

func TestKotlinExtractor(t *testing.T) {
	src := `package com.acme.orders

const val TOPIC = "orders.created"

@RestController
@RequestMapping("/api/orders")
class OrderController(private val restTemplate: RestTemplate) {

    // Creates an order and fans out.
    @PostMapping("/create")
    fun createOrder(req: CreateOrderRequest): OrderResponse {
        kafkaTemplate.send(TOPIC, req.userId)
        restTemplate.postForObject("http://billing/api/billing/charge", payload, Void::class.java)
        return process(req)
    }
}
`
	units, edges := parseOrFail(t, "kotlin", "OrderController.kt", src)

	cls := findUnit(units, "class", "OrderController")
	if cls == nil {
		t.Fatalf("missing class unit; units: %+v", names(units))
	}
	if cls.Qualified != "com.acme.orders.OrderController" {
		t.Errorf("class qualified = %q", cls.Qualified)
	}
	fn := findUnit(units, "method", "createOrder")
	if fn == nil {
		t.Fatalf("missing method unit; units: %+v", names(units))
	}
	if fn.Qualified != "com.acme.orders.OrderController.createOrder" {
		t.Errorf("method qualified = %q", fn.Qualified)
	}
	if fn.Signature != "(req: CreateOrderRequest)" {
		t.Errorf("method signature = %q", fn.Signature)
	}

	// @PostMapping on the function joined with the class @RequestMapping.
	if findUnit(units, store.KindHTTPRoute, "POST /api/orders/create") == nil {
		t.Errorf("missing route unit; units: %+v", names(units))
	}
	if findEdge(edges, store.EdgeHandledBy, "createOrder") == nil {
		t.Errorf("missing handled_by edge; edges: %+v", edgeNames(edges))
	}

	// kafkaTemplate.send with a const topic.
	if findEdge(edges, store.EdgeProduces, "topic:orders.created") == nil {
		t.Errorf("missing produces edge; edges: %+v", edgeNames(edges))
	}
	// RestTemplate call.
	if findEdge(edges, store.EdgeHTTPCall, "http:POST /api/billing/charge") == nil {
		t.Errorf("missing http_call edge; edges: %+v", edgeNames(edges))
	}
	// Plain call edge fallback.
	if findEdge(edges, store.EdgeCall, "process") == nil {
		t.Errorf("missing call edge; edges: %+v", edgeNames(edges))
	}
}

func TestKotlinWebClientChain(t *testing.T) {
	src := `package com.acme.notify

class Notifier {
    fun send(userId: String) {
        webClient.post().uri("/api/notify/send").bodyValue(body).retrieve()
    }
}
`
	_, edges := parseOrFail(t, "kotlin", "Notifier.kt", src)

	if findEdge(edges, store.EdgeHTTPCall, "http:POST /api/notify/send") == nil {
		t.Errorf("missing http_call edge; edges: %+v", edgeNames(edges))
	}
}
