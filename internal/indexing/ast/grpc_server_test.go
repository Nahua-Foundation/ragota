package ast

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// A service implementation is only visible through the generated base class it
// derives from: the .proto is compiled at build time and the registration call
// usually lives in another file. These are the shapes each language's codegen
// leaves behind, taken from Online Boutique and dotnet/eShop.

func TestGeneratedServerBaseImplementsRPC(t *testing.T) {
	tests := []struct {
		name string
		lang string
		file string
		src  string
		want []string // implements_rpc destinations
		not  []string // destinations that must not appear
	}{
		{
			name: "go struct embedding the generated Unimplemented server",
			lang: "go", file: "product_catalog.go",
			src: `package main

type productCatalog struct {
	pb.UnimplementedProductCatalogServiceServer
	catalog pb.ListProductsResponse
}

func (p *productCatalog) ListProducts(ctx context.Context, req *pb.Empty) (*pb.ListProductsResponse, error) {
	return nil, nil
}

func (p *productCatalog) GetProduct(ctx context.Context, req *pb.GetProductRequest) (*pb.Product, error) {
	return nil, nil
}

func (p *productCatalog) parseCatalog() error { return nil }
`,
			want: []string{
				GrpcKey("ProductCatalogService", "ListProducts"),
				GrpcKey("ProductCatalogService", "GetProduct"),
			},
			not: []string{GrpcKey("ProductCatalogService", "parseCatalog")},
		},
		{
			name: "go embed without a package qualifier, declared after its methods",
			lang: "go", file: "main.go",
			src: `package main

func (s *server) GetQuote(ctx context.Context, in *pb.GetQuoteRequest) (*pb.GetQuoteResponse, error) {
	return nil, nil
}

type server struct {
	UnimplementedShippingServiceServer
}
`,
			want: []string{GrpcKey("ShippingService", "GetQuote")},
		},
		{
			name: "csharp nested generated base, primary constructor",
			lang: "csharp", file: "BasketService.cs",
			src: `namespace eShop.Basket.API.Grpc;

public class BasketService(
    IBasketRepository repository,
    ILogger<BasketService> logger) : Basket.BasketBase
{
    [AllowAnonymous]
    public override async Task<CustomerBasketResponse> GetBasket(GetBasketRequest request, ServerCallContext context)
    {
        return new();
    }

    public override async Task<DeleteBasketResponse> DeleteBasket(DeleteBasketRequest request, ServerCallContext context)
    {
        return new();
    }

    private static void ThrowNotAuthenticated() { }
}
`,
			want: []string{
				GrpcKey("Basket", "GetBasket"),
				GrpcKey("Basket", "DeleteBasket"),
			},
			not: []string{GrpcKey("Basket", "ThrowNotAuthenticated")},
		},
		{
			name: "csharp fully qualified generated base",
			lang: "csharp", file: "CartService.cs",
			src: `namespace cartservice.services;

public class CartService : Hipstershop.CartService.CartServiceBase
{
    public async override Task<Empty> AddItem(AddItemRequest request, ServerCallContext context)
    {
        return new Empty();
    }
}
`,
			want: []string{GrpcKey("CartService", "AddItem")},
		},
		{
			name: "csharp bare base left by a using static, corroborated by the context type",
			lang: "csharp", file: "HealthCheckService.cs",
			src: `using static Grpc.Health.V1.Health;

namespace cartservice.services;

internal class HealthCheckService : HealthBase
{
    public override Task<HealthCheckResponse> Check(HealthCheckRequest request, ServerCallContext context)
    {
        return Task.FromResult(new HealthCheckResponse());
    }
}
`,
			want: []string{GrpcKey("Health", "Check")},
		},
		{
			name: "csharp ControllerBase is not a generated server",
			lang: "csharp", file: "OrdersController.cs",
			src: `namespace Api;

public class OrdersController : ControllerBase
{
    public override Task<string> Index(int id) { return null; }
}
`,
			not: []string{GrpcKey("Controller", "Index")},
		},
		{
			name: "java nested ImplBase, lowerCamel method mapped to the proto name",
			lang: "java", file: "AdService.java",
			src: `package hipstershop;

public final class AdService {
  private static class AdServiceImpl extends hipstershop.AdServiceGrpc.AdServiceImplBase {
    @Override
    public void getAds(AdRequest req, StreamObserver<AdResponse> responseObserver) {
    }

    public void helper(String s) {
    }
  }
}
`,
			want: []string{GrpcKey("AdService", "GetAds")},
			not:  []string{GrpcKey("AdService", "Helper")},
		},
		{
			name: "python servicer base",
			lang: "python", file: "recommendation_server.py",
			src: `class RecommendationService(demo_pb2_grpc.RecommendationServiceServicer):
    def ListRecommendations(self, request, context):
        return None

    def _helper(self):
        return None
`,
			want: []string{GrpcKey("RecommendationService", "ListRecommendations")},
		},
		{
			name: "python servicer reached through an intermediate base in the same file",
			lang: "python", file: "email_server.py",
			src: `class BaseEmailService(demo_pb2_grpc.EmailServiceServicer):
    def Check(self, request, context):
        return None


class DummyEmailService(BaseEmailService):
    def SendOrderConfirmation(self, request, context):
        return None
`,
			want: []string{
				GrpcKey("EmailService", "Check"),
				GrpcKey("EmailService", "SendOrderConfirmation"),
			},
		},
		{
			name: "node service definition with a shorthand handler map",
			lang: "javascript", file: "server.js",
			src: `function getSupportedCurrencies (call, callback) {}
function convert (call, callback) {}

function main () {
  const server = new grpc.Server();
  server.addService(shopProto.CurrencyService.service, {getSupportedCurrencies, convert});
}
`,
			want: []string{
				GrpcKey("CurrencyService", "GetSupportedCurrencies"),
				GrpcKey("CurrencyService", "Convert"),
			},
		},
		{
			name: "node handler map whose values are bound statics",
			lang: "javascript", file: "server.js",
			src: `class HipsterShopServer {
  static ChargeServiceHandler(call, callback) {}

  listen () {
    this.server.addService(
      hipsterShopPackage.PaymentService.service,
      {
        charge: HipsterShopServer.ChargeServiceHandler.bind(this)
      }
    );
  }
}
`,
			want: []string{GrpcKey("PaymentService", "Charge")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := parseFactsOrFail(t, tt.lang, tt.file, tt.src)
			for _, want := range tt.want {
				if findEdge(facts.Edges, storage.EdgeImplementsRPC, want) == nil {
					t.Errorf("implements_rpc %q missing from %v", want, edgeNamesOfKind(facts.Edges, storage.EdgeImplementsRPC))
				}
			}
			for _, not := range tt.not {
				if findEdge(facts.Edges, storage.EdgeImplementsRPC, not) != nil {
					t.Errorf("implements_rpc %q was emitted and should not be", not)
				}
			}
		})
	}
}

func TestCsharpGRPCServiceFromBase(t *testing.T) {
	tests := []struct {
		base    string
		hasGRPC bool
		want    string
	}{
		{base: "Basket.BasketBase", want: "Basket"},
		{base: "Hipstershop.CartService.CartServiceBase", want: "CartService"},
		{base: "HealthBase", hasGRPC: true, want: "Health"},
		{base: "HealthBase", hasGRPC: false, want: ""},
		{base: "ControllerBase", hasGRPC: false, want: ""},
		{base: "DbContext", hasGRPC: true, want: ""},
		{base: "IIntegrationEventHandler<OrderStarted>", hasGRPC: true, want: ""},
	}
	for _, tt := range tests {
		if got := csharpGRPCServiceFromBase(tt.base, tt.hasGRPC); got != tt.want {
			t.Errorf("csharpGRPCServiceFromBase(%q, %v) = %q, want %q", tt.base, tt.hasGRPC, got, tt.want)
		}
	}
}

func TestJavaGRPCServiceFromBase(t *testing.T) {
	tests := []struct{ superclass, want string }{
		{"extends hipstershop.AdServiceGrpc.AdServiceImplBase", "AdService"},
		{"extends OrderServiceGrpc.OrderServiceImplBase", "OrderService"},
		{"extends GreeterImplBase", "Greeter"},
		{"extends AbstractController", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := javaGRPCServiceFromBase(tt.superclass); got != tt.want {
			t.Errorf("javaGRPCServiceFromBase(%q) = %q, want %q", tt.superclass, got, tt.want)
		}
	}
}

func TestGoEmbeddedServer(t *testing.T) {
	tests := []struct{ decl, want string }{
		{"server struct {\n\tpb.UnimplementedShippingServiceServer\n}", "ShippingService"},
		{"s struct {\n\tUnimplementedCheckoutServiceServer\n\taddr string\n}", "CheckoutService"},
		{"s struct {\n\tsrv UnimplementedFooServer\n}", ""},
		{"s struct {\n\taddr string\n}", ""},
	}
	for _, tt := range tests {
		if got := goEmbeddedServer(tt.decl); got != tt.want {
			t.Errorf("goEmbeddedServer(%q) = %q, want %q", tt.decl, got, tt.want)
		}
	}
}

func edgeNamesOfKind(edges []*storage.Edge, kind string) []string {
	var out []string
	for _, e := range edges {
		if e.Kind == kind {
			out = append(out, e.DstName)
		}
	}
	return out
}
