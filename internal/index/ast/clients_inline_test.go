package ast

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/store"
)

// A client constructed inline is the shape Online Boutique's Go frontend uses
// for every one of its calls, and the one that made it report 121
// implements_rpc against 2 rpc_call.

func TestInlineConstructedClientCalls(t *testing.T) {
	tests := []struct {
		name string
		lang string
		file string
		src  string
		want string // rpc_call edge key
	}{
		{
			name: "go generated constructor chained into the call",
			lang: "go", file: "rpc.go",
			src: `package main

func (fe *frontendServer) getCart(ctx context.Context, userID string) ([]*pb.CartItem, error) {
	resp, err := pb.NewCartServiceClient(fe.cartSvcConn).GetCart(ctx, &pb.GetCartRequest{UserId: userID})
	return resp.GetItems(), err
}
`,
			want: "grpc:CartService/GetCart",
		},
		{
			name: "go constructor and call split across lines",
			lang: "go", file: "handlers.go",
			src: `package main

func (fe *frontendServer) placeOrder(ctx context.Context) {
	order, err := pb.NewCheckoutServiceClient(fe.checkoutSvcConn).
		PlaceOrder(ctx, &pb.PlaceOrderRequest{
			Email: email,
		})
}
`,
			want: "grpc:CheckoutService/PlaceOrder",
		},
		{
			name: "java generated stub factory chained into the call",
			lang: "java", file: "OrderClient.java",
			src: `package com.acme.gateway;

public class OrderClient {
    void call(CreateOrderRequest req) {
        OrderServiceGrpc.newBlockingStub(channel).createOrder(req);
    }
}
`,
			want: "grpc:OrderService/CreateOrder",
		},
		{
			name: "csharp generated client constructed inline",
			lang: "csharp", file: "OrderCaller.cs",
			src: `namespace Acme.Gateway;

public class OrderCaller
{
    public async Task CallAsync(CreateOrderRequest req)
    {
        await new OrderService.OrderServiceClient(channel).CreateOrderAsync(req);
    }
}
`,
			want: "grpc:OrderService/CreateOrder",
		},
		{
			name: "typescript generated client constructed inline",
			lang: "typescript", file: "orders.ts",
			src: `export function createOrder(userId: string) {
  new OrderServiceClient(addr, credentials).createOrder({ user_id: userId });
}
`,
			want: "grpc:OrderService/CreateOrder",
		},
		{
			name: "python generated stub constructed inline",
			lang: "python", file: "orders.py",
			src: `import grpc

def create_order(user_id):
    return pb2_grpc.OrderServiceStub(channel).CreateOrder(request)
`,
			want: "grpc:OrderService/CreateOrder",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, edges := parseOrFail(t, tt.lang, tt.file, tt.src)
			if findEdge(edges, store.EdgeRPCCall, tt.want) == nil {
				t.Fatalf("missing rpc_call %q; edges: %+v", tt.want, edgeNames(edges))
			}
		})
	}
}

// An HTTP client constructed inline is recognized by the type it constructs,
// the same way an injected one is recognized by its declared type.
func TestInlineConstructedHTTPClient(t *testing.T) {
	src := `package com.acme.billing;

public class Notifier {
    void charge() {
        new RestTemplate().delete("http://billing/api/billing/charge/1");
    }
}
`
	_, edges := parseOrFail(t, "java", "Notifier.java", src)
	if findEdge(edges, store.EdgeHTTPCall, "http:DELETE /api/billing/charge/1") == nil {
		t.Fatalf("missing http_call edge; edges: %+v", edgeNames(edges))
	}
}

func TestInlineConstructedType(t *testing.T) {
	tests := map[string]string{
		"pb.NewCartServiceClient(fe.cartSvcConn)":  "pb.CartServiceClient",
		"NewOrderServiceClient(conn)":              "OrderServiceClient",
		"new OrderService.OrderServiceClient(ch)":  "OrderService.OrderServiceClient",
		"new HttpClient()":                         "HttpClient",
		"OrderServiceGrpc.newBlockingStub(ch)":     "OrderServiceStub",
		"pb2_grpc.OrderServiceStub(channel)":       "pb2_grpc.OrderServiceStub",
		"pb.NewCurrencyServiceClient(\n\tfe.conn)": "pb.CurrencyServiceClient",
		// Not constructors.
		"client":                   "",
		"getClient()":              "",
		"fe.cartSvcConn":           "",
		"newThing(x)":              "",
		"fmt.Sprintf(\"%s\", url)": "",
	}
	for expr, want := range tests {
		if got := inlineConstructedType(expr); got != want {
			t.Errorf("inlineConstructedType(%q) = %q, want %q", expr, got, want)
		}
	}
}

// A constructor whose type is an ordinary SDK client must not be read as a
// gRPC stub, which is the gate the type-name heuristics already apply.
func TestInlineConstructedHTTPClientIsNotAStub(t *testing.T) {
	src := `package main

func fetch() {
	NewHTTPClient(cfg).Do(req)
}
`
	_, edges := parseOrFail(t, "go", "main.go", src)
	for _, e := range edges {
		if e.Kind == store.EdgeRPCCall {
			t.Errorf("unexpected rpc_call %q", e.DstName)
		}
	}
}
