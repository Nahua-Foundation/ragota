package ast

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// Tests for the universal-shape coverage: dynamic URLs, router prefixes,
// message-struct kafka topics, injected gRPC stubs, ORM data access, async
// brokers beyond kafka, and method-in-argument HTTP clients.

// --- defect 1: python parameter lists ---------------------------------------

func TestParamNamesDropsPythonReceiver(t *testing.T) {
	tests := []struct {
		name string
		lang string
		sig  string
		want []string
	}{
		{"python method drops self", "python", "(self, user_id, amount)", []string{"user_id", "amount"}},
		{"python classmethod drops cls", "python", "(cls, user_id)", []string{"user_id"}},
		{"python function keeps all", "python", "(user_id, amount)", []string{"user_id", "amount"}},
		{"python typed and defaulted", "python", "(self, user_id: str, limit: int = 10)", []string{"user_id", "limit"}},
		{"python selfish name kept", "python", "(selfish, x)", []string{"selfish", "x"}},
		{"go keeps everything", "go", "(ctx context.Context, self string)", []string{"ctx", "self"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := contract.ParamNames(tt.lang, tt.sig)
			if len(got) != len(tt.want) {
				t.Fatalf("contract.ParamNames(%q) = %v, want %v", tt.sig, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("contract.ParamNames(%q) = %v, want %v", tt.sig, got, tt.want)
				}
			}
		})
	}
}

// --- defect 3: typescript client calls are not routes -----------------------

func TestTypeScriptHTTPClientIsNotARoute(t *testing.T) {
	src := `import { Injectable } from '@nestjs/common';

@Injectable()
export class UserService {
  constructor(private http: HttpClient) {}

  list(headers: any) {
    return this.http.get('/api/users', { headers });
  }
}
`
	units, edges := parseOrFail(t, "typescript", "user.app.ts", src)

	if u := findUnit(units, store.KindHTTPRoute, "GET /api/users"); u != nil {
		t.Errorf("client call registered a phantom route: %+v", names(units))
	}
	if findEdge(edges, store.EdgeHTTPCall, "http:GET /api/users") == nil {
		t.Errorf("missing outgoing http_call; edges: %+v", edgeNames(edges))
	}
}

func TestTypeScriptExpressRouteStillDetected(t *testing.T) {
	src := `const router = express.Router();

router.get('/orders', (req, res) => res.json([]));
router.post('/orders', authMiddleware, createOrder);
router.put('/orders/:id', asyncHandler(updateOrder));
`
	units, edges := parseOrFail(t, "typescript", "routes.ts", src)

	for _, want := range []string{"GET /orders", "POST /orders", "PUT /orders/:id"} {
		if findUnit(units, store.KindHTTPRoute, want) == nil {
			t.Errorf("missing route %q; units: %+v", want, names(units))
		}
	}
	if findEdge(edges, store.EdgeHandledBy, "createOrder") == nil {
		t.Errorf("missing handled_by edge; edges: %+v", edgeNames(edges))
	}
}

// --- defect 4: proto single-line blocks -------------------------------------

func TestProtoSingleLineBlocksCloseScope(t *testing.T) {
	src := `syntax = "proto3";
package orders.v1;

message Empty {}

enum Status { UNKNOWN = 0; ACTIVE = 1; }

message CreateOrderRequest {
  string user_id = 1;
}

service OrderService {
  rpc CreateOrder(CreateOrderRequest) returns (Empty);
}
`
	units, _ := parseOrFail(t, "proto", "orders.proto", src)

	// The field must be attributed to CreateOrderRequest, not to Empty.
	f := findUnit(units, store.KindProtoField, "user_id")
	if f == nil {
		t.Fatalf("missing proto_field; units: %+v", names(units))
	}
	if f.Qualified != "proto:orders.v1.CreateOrderRequest.user_id" {
		t.Errorf("field qualified = %q, want proto:orders.v1.CreateOrderRequest.user_id", f.Qualified)
	}
	// The rpc must still be found: the service scope must be open.
	m := findUnit(units, store.KindRPCMethod, "CreateOrder")
	if m == nil {
		t.Fatalf("missing rpc_method; units: %+v", names(units))
	}
	if m.Qualified != "grpc:orders.v1.OrderService/CreateOrder" {
		t.Errorf("rpc qualified = %q", m.Qualified)
	}
}

// --- gap 12: grpc-gateway http bindings -------------------------------------

func TestProtoGrpcGatewayRoutes(t *testing.T) {
	src := `syntax = "proto3";
package orders.v1;

import "google/api/annotations.proto";

service OrderService {
  rpc GetOrder(GetOrderRequest) returns (Order) {
    option (google.api.http) = {
      get: "/v1/orders/{order_id}"
    };
  }
  rpc CreateOrder(CreateOrderRequest) returns (Order) {
    option (google.api.http) = { post: "/v1/orders" body: "*" };
  }
}

message Order {
  string id = 1;
}
`
	units, edges := parseOrFail(t, "proto", "orders.proto", src)

	if findUnit(units, store.KindHTTPRoute, "GET /v1/orders/{order_id}") == nil {
		t.Errorf("missing gateway GET route; units: %+v", names(units))
	}
	if findUnit(units, store.KindHTTPRoute, "POST /v1/orders") == nil {
		t.Errorf("missing gateway POST route; units: %+v", names(units))
	}
	if findEdge(edges, store.EdgeHandledBy, "GetOrder") == nil {
		t.Errorf("missing handled_by edge to the rpc; edges: %+v", edgeNames(edges))
	}
	// The multi-line option block must not close the service scope early.
	if findUnit(units, store.KindProtoField, "id") == nil {
		t.Errorf("message after an option block was lost; units: %+v", names(units))
	}
}

// --- defect 6: named confidence tiers ---------------------------------------

func TestEdgeConfidencesUseNamedTiers(t *testing.T) {
	_, edges := parseOrFail(t, "proto", "o.proto", `package p;
service S {
  rpc Do(Req) returns (Resp);
}
`)
	e := findEdge(edges, store.EdgeRPCRequest, "proto:p.Req")
	if e == nil {
		t.Fatalf("missing rpc_request edge; edges: %+v", edgeNames(edges))
	}
	if e.Confidence != contract.ConfExact {
		t.Errorf("rpc_request confidence = %v, want ConfExact", e.Confidence)
	}

	_, edges = parseOrFail(t, "go", "main.go", `package main

func save(db *sql.DB, id string) {
	db.Exec("INSERT INTO orders (id) VALUES ($1)", id)
}
`)
	e = findEdge(edges, store.EdgeWritesTo, "db:orders")
	if e == nil {
		t.Fatalf("missing writes_to edge; edges: %+v", edgeNames(edges))
	}
	if e.Confidence != contract.ConfHigh {
		t.Errorf("writes_to confidence = %v, want ConfHigh", e.Confidence)
	}
}

// --- gap 7: dynamic URLs ----------------------------------------------------

func TestInterpolatedPath(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want string
		ok   bool
	}{
		{"go sprintf", `fmt.Sprintf("/orders/%d", id)`, "/orders/{}", true},
		{"go sprintf base", `fmt.Sprintf("%s/orders/%s", base, id)`, "/orders/{}", true},
		{"go concat suffix", `baseURL + "/orders"`, "/orders", true},
		{"go concat value", `"/orders/" + id`, "/orders/{}", true},
		{"go concat middle", `"/orders/" + id + "/items"`, "/orders/{}/items", true},
		{"js template", "`${base}/orders/${id}`", "/orders/{}", true},
		{"py fstring", `f"/orders/{order_id}"`, "/orders/{order_id}", true},
		{"py percent", `"/orders/%s" % oid`, "/orders/{}", true},
		{"py format", `"/orders/{}".format(oid)`, "/orders/{}", true},
		{"absolute url", `fmt.Sprintf("http://billing/api/charge/%s", id)`, "http://billing/api/charge/{}", true},
		{"query stripped", `"/orders?since=" + ts`, "/orders", true},
		{"no literal", `baseURL + path`, "", false},
		{"all placeholders", `fmt.Sprintf("/%s", x)`, "", false},
		{"sql text", `"SELECT * FROM orders WHERE id = " + id`, "", false},
		{"not a path", `"user:" + id`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := interpolatedPath(tt.expr)
			if ok != tt.ok || got != tt.want {
				t.Errorf("interpolatedPath(%s) = (%q, %v), want (%q, %v)", tt.expr, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestDynamicURLsProduceHTTPCalls(t *testing.T) {
	tests := []struct {
		name string
		lang string
		file string
		src  string
		want string
	}{
		{
			name: "go sprintf",
			lang: "go", file: "client.go",
			src: `package main

func get(id int) {
	http.Get(fmt.Sprintf("http://orders/api/orders/%d", id))
}
`,
			want: "http:GET /api/orders/{}",
		},
		{
			name: "go new request concat",
			lang: "go", file: "client.go",
			src: `package main

func send(base, id string) {
	req, _ := http.NewRequest(http.MethodPost, base+"/api/orders/"+id, body)
}
`,
			want: "http:POST /api/orders/{}",
		},
		{
			name: "python fstring",
			lang: "python", file: "client.py",
			src: `import requests

def get_order(order_id):
    return requests.get(f"http://orders/api/orders/{order_id}")
`,
			want: "http:GET /api/orders/{}",
		},
		{
			name: "typescript template literal",
			lang: "typescript", file: "client.ts",
			src:  "async function getOrder(id: string) {\n  return axios.get(`${BASE}/api/orders/${id}`);\n}\n",
			want: "http:GET /api/orders/{}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, edges := parseOrFail(t, tt.lang, tt.file, tt.src)
			e := findEdge(edges, store.EdgeHTTPCall, tt.want)
			if e == nil {
				t.Fatalf("missing http_call %q; edges: %+v", tt.want, edgeNames(edges))
			}
			if e.Confidence != contract.ConfHeuristic {
				t.Errorf("dynamic URL confidence = %v, want ConfHeuristic", e.Confidence)
			}
		})
	}
}

// --- gap 8: router prefixes, groups and mounts ------------------------------

func TestGoRouterPrefixes(t *testing.T) {
	src := `package main

func routes(r chi.Router, g *gin.Engine) {
	r.Route("/api", func(r chi.Router) {
		r.Get("/orders", listOrders)
		r.Route("/admin", func(r chi.Router) {
			r.Post("/purge", purge)
		})
	})

	v1 := g.Group("/api/v1")
	v1.GET("/users", listUsers)
}
`
	units, _ := parseOrFail(t, "go", "routes.go", src)

	for _, want := range []string{"GET /api/orders", "POST /api/admin/purge", "GET /api/v1/users"} {
		if findUnit(units, store.KindHTTPRoute, want) == nil {
			t.Errorf("missing route %q; units: %+v", want, names(units))
		}
	}
}

func TestTypeScriptRouterMountPrefix(t *testing.T) {
	src := `const router = express.Router();
router.get('/orders', listOrders);

const app = express();
app.use('/api', router);
`
	units, _ := parseOrFail(t, "typescript", "app.ts", src)

	if findUnit(units, store.KindHTTPRoute, "GET /api/orders") == nil {
		t.Errorf("missing mounted route; units: %+v", names(units))
	}
}

func TestPythonRouterPrefixes(t *testing.T) {
	src := `from fastapi import APIRouter
from flask import Blueprint

router = APIRouter(prefix="/api/v1")
bp = Blueprint("orders", __name__, url_prefix="/api")

@router.get("/orders")
def list_orders():
    return []

@bp.route("/legacy", methods=["POST"])
def legacy():
    return ""
`
	units, _ := parseOrFail(t, "python", "routes.py", src)

	if findUnit(units, store.KindHTTPRoute, "GET /api/v1/orders") == nil {
		t.Errorf("missing APIRouter-prefixed route; units: %+v", names(units))
	}
	if findUnit(units, store.KindHTTPRoute, "POST /api/legacy") == nil {
		t.Errorf("missing Blueprint-prefixed route; units: %+v", names(units))
	}
}

func TestNestJSControllerRoutes(t *testing.T) {
	src := `import { Controller, Get, Post } from '@nestjs/common';

@Controller('orders')
export class OrdersController {
  @Get(':id')
  findOne(id: string) { return null; }

  @Post()
  create(body: any) { return null; }

  @EventPattern('order.created')
  handleCreated(payload: any) {}
}
`
	units, edges := parseOrFail(t, "typescript", "orders.controller.ts", src)

	if findUnit(units, store.KindHTTPRoute, "GET /orders/:id") == nil {
		t.Errorf("missing @Get route; units: %+v", names(units))
	}
	if findUnit(units, store.KindHTTPRoute, "POST /orders") == nil {
		t.Errorf("missing @Post route; units: %+v", names(units))
	}
	if findEdge(edges, store.EdgeConsumes, "topic:order.created") == nil {
		t.Errorf("missing @EventPattern consumes edge; edges: %+v", edgeNames(edges))
	}
}

// --- gap 9: kafka topic carried in the message struct -----------------------

func TestGoKafkaTopicInMessageStruct(t *testing.T) {
	src := `package main

func publish(producer sarama.SyncProducer) {
	producer.SendMessage(&sarama.ProducerMessage{
		Topic: "orders.created",
		Value: sarama.StringEncoder(payload),
	})
}
`
	_, edges := parseOrFail(t, "go", "sarama.go", src)

	e := findEdge(edges, store.EdgeProduces, "topic:orders.created")
	if e == nil {
		t.Fatalf("missing sarama produces edge; edges: %+v", edgeNames(edges))
	}
	if e.Confidence != contract.ConfHigh {
		t.Errorf("confidence = %v, want ConfHigh", e.Confidence)
	}
}

func TestGoKafkaConfluentTopicPointer(t *testing.T) {
	src := `package main

const ordersTopic = "orders.created"

func publish(p *kafka.Producer) {
	topic := ordersTopic
	p.Produce(&kafka.Message{
		TopicPartition: kafka.TopicPartition{Topic: &topic, Partition: kafka.PartitionAny},
		Value:          payload,
	}, nil)
}
`
	_, edges := parseOrFail(t, "go", "confluent.go", src)

	if findEdge(edges, store.EdgeProduces, "topic:orders.created") == nil {
		t.Errorf("missing confluent produces edge; edges: %+v", edgeNames(edges))
	}
}

func TestGoKafkaSubscribeTopics(t *testing.T) {
	src := `package main

func consume(c *kafka.Consumer) {
	c.SubscribeTopics([]string{"orders.created", "orders.shipped"}, nil)
}
`
	_, edges := parseOrFail(t, "go", "consumer.go", src)

	for _, want := range []string{"topic:orders.created", "topic:orders.shipped"} {
		if findEdge(edges, store.EdgeConsumes, want) == nil {
			t.Errorf("missing consumes %q; edges: %+v", want, edgeNames(edges))
		}
	}
}

// --- gap 10: dependency-injected gRPC clients -------------------------------

func TestInjectedGRPCClients(t *testing.T) {
	tests := []struct {
		name string
		lang string
		file string
		src  string
	}{
		{
			name: "go struct field", lang: "go", file: "handler.go",
			src: `package handler

type Handler struct {
	orders pb.OrderServiceClient
}

func (h *Handler) Create(ctx context.Context, userID string) {
	h.orders.CreateOrder(ctx, &pb.CreateOrderRequest{UserId: userID})
}
`,
		},
		{
			name: "java injected stub", lang: "java", file: "Gateway.java",
			src: `package com.acme;

public class Gateway {
    private final OrderServiceGrpc.OrderServiceBlockingStub orders;

    public void create(CreateOrderRequest req) {
        orders.createOrder(req);
    }
}
`,
		},
		{
			name: "csharp injected client", lang: "csharp", file: "Gateway.cs",
			src: `namespace Acme;

public class Gateway
{
    private readonly OrderService.OrderServiceClient _orders;

    public async Task Create(CreateOrderRequest req)
    {
        await _orders.CreateOrderAsync(req);
    }
}
`,
		},
		{
			name: "typescript constructor injection", lang: "typescript", file: "gateway.ts",
			src: `export class Gateway {
  constructor(private orders: OrderServiceClient) {}

  create(req: CreateOrderRequest) {
    return this.orders.createOrder(req);
  }
}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, edges := parseOrFail(t, tt.lang, tt.file, tt.src)
			e := findEdge(edges, store.EdgeRPCCall, "grpc:OrderService/CreateOrder")
			if e == nil {
				t.Fatalf("missing rpc_call edge; edges: %+v", edgeNames(edges))
			}
			if e.Confidence != contract.ConfCrossFile {
				t.Errorf("confidence = %v, want ConfCrossFile", e.Confidence)
			}
		})
	}
}

func TestInjectedHTTPClientIsNotAGRPCStub(t *testing.T) {
	src := `export class Api {
  constructor(private http: HttpClient) {}

  load() {
    return this.http.get('/api/things');
  }
}
`
	_, edges := parseOrFail(t, "typescript", "api.ts", src)

	if findEdge(edges, store.EdgeHTTPCall, "http:GET /api/things") == nil {
		t.Errorf("HttpClient must stay an http_call; edges: %+v", edgeNames(edges))
	}
	for _, e := range edges {
		if e.Kind == store.EdgeRPCCall {
			t.Errorf("HttpClient must not produce an rpc_call: %+v", e)
		}
	}
}

// --- gap 11: placeholder defaults -------------------------------------------

func TestSplitPlaceholderDefault(t *testing.T) {
	tests := []struct {
		in   string
		key  string
		def  string
		okay bool
	}{
		{"${ORDERS_TOPIC:orders}", "ORDERS_TOPIC", "orders", true},
		{"${ORDERS_TOPIC:-orders}", "ORDERS_TOPIC", "orders", true},
		{"${ORDERS_TOPIC}", "ORDERS_TOPIC", "", false},
		{"orders", "", "", false},
	}
	for _, tt := range tests {
		key, def, ok := splitPlaceholderDefault(tt.in)
		if key != tt.key || def != tt.def || ok != tt.okay {
			t.Errorf("splitPlaceholderDefault(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tt.in, key, def, ok, tt.key, tt.def, tt.okay)
		}
	}
}

func TestConfigPlaceholderDefaultsResolve(t *testing.T) {
	units, _, err := GetParserForLanguage("yaml").Parse("application.yml", `
kafka:
  orders-topic: ${ORDERS_TOPIC:orders.created}
  brokers: ${KAFKA_BROKERS}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := map[string]string{}
	for _, u := range units {
		got[u.Qualified] = u.Signature
	}
	if got["config:kafka.orders-topic"] != "orders.created" {
		t.Errorf("placeholder default not applied: %q", got["config:kafka.orders-topic"])
	}
	if got["config:kafka.brokers"] != "${KAFKA_BROKERS}" {
		t.Errorf("plain reference must be left for the linker: %q", got["config:kafka.brokers"])
	}
}

func TestKafkaListenerPlaceholderDefault(t *testing.T) {
	src := `package com.acme;

public class Listener {
    @KafkaListener(topics = "${orders.topic:orders.created}")
    public void onOrder(String payload) {}
}
`
	_, edges := parseOrFail(t, "java", "Listener.java", src)

	e := findEdge(edges, store.EdgeConsumes, "topic:${orders.topic}")
	if e == nil {
		t.Fatalf("topic ref must drop the default so config resolution can match; edges: %+v", edgeNames(edges))
	}
	if got := store.DecodeEdgeMeta(e.Meta).Topic; got != "orders.created" {
		t.Errorf("meta topic = %q, want the default value orders.created", got)
	}
}

// --- gap 13: openapi servers base path --------------------------------------

func TestOpenAPIServerBasePath(t *testing.T) {
	units, _, err := GetParserForLanguage("yaml").Parse("openapi.yml", `
openapi: 3.0.0
servers:
  - url: https://api.example.com/api/v1
paths:
  /orders:
    get:
      summary: List orders
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if findUnit(units, store.KindHTTPRoute, "GET /api/v1/orders") == nil {
		t.Errorf("route must carry the server base path; units: %+v", names(units))
	}
}

func TestSwaggerBasePath(t *testing.T) {
	units, _, err := GetParserForLanguage("json").Parse("swagger.json",
		`{"swagger":"2.0","basePath":"/api/v2","paths":{"/orders":{"post":{}}}}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if findUnit(units, store.KindHTTPRoute, "POST /api/v2/orders") == nil {
		t.Errorf("swagger basePath ignored; units: %+v", names(units))
	}
}

// --- gap 14: ORM data access ------------------------------------------------

func TestTableNameDerivation(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Order", "orders"},
		{"OrderItem", "order_items"},
		{"Address", "addresses"},
		{"Company", "companies"},
		{"Box", "boxes"},
		{"pb.Order", "orders"},
	}
	for _, tt := range tests {
		if got := tableName(tt.in); got != tt.want {
			t.Errorf("tableName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGoGormDataAccess(t *testing.T) {
	src := `package repo

type Order struct {
	ID     string ` + "`gorm:\"primaryKey\"`" + `
	UserID string ` + "`gorm:\"index\"`" + `
}

func Save(db *gorm.DB, o *Order) {
	db.Create(o)
}

func Ship(db *gorm.DB, id string) {
	db.Model(&Order{}).Where("id = ?", id).Updates(map[string]any{"status": "shipped"})
}

func List(db *gorm.DB) {
	var orders []Order
	db.Table("orders").Find(&orders)
}
`
	units, edges := parseOrFail(t, "go", "repo.go", src)

	if findUnit(units, store.KindDBTable, "orders") == nil {
		t.Errorf("gorm-tagged struct must publish its table; units: %+v", names(units))
	}
	if findEdge(edges, store.EdgeWritesTo, "db:orders") == nil {
		t.Errorf("missing writes_to edge; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeReadsFrom, "db:orders") == nil {
		t.Errorf("missing reads_from edge; edges: %+v", edgeNames(edges))
	}
}

func TestJavaJPAEntityAndRepository(t *testing.T) {
	src := `package com.acme;

@Entity
@Table(name = "orders")
public class Order {
    private String id;
}

class OrderService {
    private final OrderRepository orderRepository;

    void create(Order o) {
        orderRepository.save(o);
    }

    void load(String id) {
        orderRepository.findById(id);
    }
}
`
	units, edges := parseOrFail(t, "java", "Order.java", src)

	if findUnit(units, store.KindDBTable, "orders") == nil {
		t.Errorf("@Table must publish the table unit; units: %+v", names(units))
	}
	if findEdge(edges, store.EdgeWritesTo, "db:orders") == nil {
		t.Errorf("repository save must write; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeReadsFrom, "db:orders") == nil {
		t.Errorf("repository findById must read; edges: %+v", edgeNames(edges))
	}
}

func TestCSharpEFCoreDbSet(t *testing.T) {
	src := `namespace Acme;

public class ShopContext : DbContext
{
    public DbSet<Order> Orders { get; set; }
}

public class OrderService
{
    private readonly ShopContext _context;

    public void Create(Order order)
    {
        _context.Orders.Add(order);
    }

    public Order Load(string id)
    {
        return _context.Orders.FirstOrDefault(o => o.Id == id);
    }
}
`
	_, edges := parseOrFail(t, "csharp", "ShopContext.cs", src)

	if findEdge(edges, store.EdgeWritesTo, "db:orders") == nil {
		t.Errorf("DbSet Add must write; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeReadsFrom, "db:orders") == nil {
		t.Errorf("DbSet FirstOrDefault must read; edges: %+v", edgeNames(edges))
	}
}

func TestPythonSQLAlchemyModel(t *testing.T) {
	src := `from sqlalchemy import Column, String

class Order(Base):
    __tablename__ = "orders"
    id = Column(String, primary_key=True)

def create(session, order):
    session.add(order)

def load(session):
    return session.query(Order).all()
`
	units, edges := parseOrFail(t, "python", "models.py", src)

	if findUnit(units, store.KindDBTable, "orders") == nil {
		t.Errorf("__tablename__ must publish the table; units: %+v", names(units))
	}
	if findEdge(edges, store.EdgeReadsFrom, "db:orders") == nil {
		t.Errorf("session.query must read; edges: %+v", edgeNames(edges))
	}
}

func TestTypeScriptPrismaAccess(t *testing.T) {
	src := `export class OrderService {
  async list() {
    return prisma.order.findMany();
  }

  async create(data: any) {
    return prisma.orderItem.create({ data });
  }
}
`
	_, edges := parseOrFail(t, "typescript", "orders.app.ts", src)

	if findEdge(edges, store.EdgeReadsFrom, "db:order") == nil {
		t.Errorf("prisma findMany must read; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeWritesTo, "db:order_item") == nil {
		t.Errorf("prisma create must write; edges: %+v", edgeNames(edges))
	}
}

// --- gap 15: async brokers beyond kafka -------------------------------------

func TestPythonAiokafkaAndCelery(t *testing.T) {
	src := `from aiokafka import AIOKafkaProducer
from celery import shared_task

async def publish(producer, payload):
    await producer.send_and_wait("orders.created", payload)

@shared_task
def process_order(order_id):
    pass

def dispatch(order_id):
    process_order.delay(order_id)
`
	_, edges := parseOrFail(t, "python", "tasks.py", src)

	if findEdge(edges, store.EdgeProduces, "topic:orders.created") == nil {
		t.Errorf("missing aiokafka produces edge; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeConsumes, "topic:process_order") == nil {
		t.Errorf("missing celery task consumes edge; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeProduces, "topic:process_order") == nil {
		t.Errorf("missing celery delay produces edge; edges: %+v", edgeNames(edges))
	}
}

func TestPythonFaustAgent(t *testing.T) {
	src := `import faust

app = faust.App("orders")
orders_topic = app.topic("orders.created")

@app.agent(orders_topic)
async def process(stream):
    async for event in stream:
        pass
`
	_, edges := parseOrFail(t, "python", "agents.py", src)

	if findEdge(edges, store.EdgeConsumes, "topic:orders.created") == nil {
		t.Errorf("missing faust agent consumes edge; edges: %+v", edgeNames(edges))
	}
}

func TestTypeScriptBullMQ(t *testing.T) {
	src := `import { Queue, Worker } from 'bullmq';

const queue = new Queue('orders');

export async function enqueue(data: any) {
  await queue.add('createOrder', data);
}

export function start() {
  const worker = new Worker('orders', async job => process(job));
}
`
	_, edges := parseOrFail(t, "typescript", "queue.ts", src)

	if findEdge(edges, store.EdgeProduces, "topic:orders") == nil {
		t.Errorf("missing bullmq produces edge; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeConsumes, "topic:orders") == nil {
		t.Errorf("missing bullmq worker consumes edge; edges: %+v", edgeNames(edges))
	}
}

// --- gap 16: method-in-argument HTTP clients --------------------------------

func TestPythonRequestsRequest(t *testing.T) {
	src := `import requests

def call(payload):
    requests.request("POST", "http://billing/api/billing/charge", json=payload)
`
	_, edges := parseOrFail(t, "python", "client.py", src)

	e := findEdge(edges, store.EdgeHTTPCall, "http:POST /api/billing/charge")
	if e == nil {
		t.Fatalf("missing http_call; edges: %+v", edgeNames(edges))
	}
	if m := store.DecodeEdgeMeta(e.Meta); m.Host != "billing" || m.Method != "POST" {
		t.Errorf("meta = %+v", m)
	}
}

func TestJavaRestTemplateExecute(t *testing.T) {
	src := `package com.acme;

class Client {
    void call() {
        restTemplate.execute("http://orders/api/orders", HttpMethod.GET, requestCallback, extractor);
    }
}
`
	_, edges := parseOrFail(t, "java", "Client.java", src)

	if findEdge(edges, store.EdgeHTTPCall, "http:GET /api/orders") == nil {
		t.Errorf("missing http_call for execute(); edges: %+v", edgeNames(edges))
	}
}

// TestGoRouteFromConstantAndWrapper covers how real Go services register
// routes: a package constant, optionally wrapped in a helper that prefixes a
// base path (nats-server's mux.HandleFunc(s.basePath(VarzPath), ...)).
func TestGoRouteFromConstantAndWrapper(t *testing.T) {
	src := `package server

const VarzPath = "/varz"
const ConnzPath = "/connz"

func (s *Server) start() {
	mux := http.NewServeMux()
	mux.HandleFunc(VarzPath, s.HandleVarz)
	mux.HandleFunc(s.basePath(ConnzPath), s.HandleConnz)
}
`
	units, _ := parseOrFail(t, "go", "server/server.go", src)

	for _, want := range []string{"ANY /varz", "ANY /connz"} {
		if findUnit(units, store.KindHTTPRoute, want) == nil {
			t.Errorf("route %q not detected; units: %v", want, names(units))
		}
	}
}

// TestBrokerFallbackNeedsADestination pins the coverage counter's honesty: a
// receiver merely named like a publisher is not a messaging site. Consul's
// in-process event stream reported 71 missed contracts on that basis alone,
// which is the "we did not find it" reading the report exists to make
// trustworthy.
func TestBrokerFallbackNeedsADestination(t *testing.T) {
	src := `package server

func (s *Server) publishEvents() {
	s.pub.Publish([]stream.Event{{Topic: EventTopic}})
	s.Publisher.Subscribe(&stream.SubscribeRequest{})
}

func (s *Server) realBroker() {
	s.brokerClient.Publish("orders.created", payload)
}
`
	units, edges := parseOrFail(t, "go", "server/publish.go", src)
	_ = units

	if e := findEdge(edges, store.EdgeProduces, "topic:orders.created"); e == nil {
		t.Errorf("a broker publish with a destination literal must still be detected; edges: %v", edgeNames(edges))
	}
	for _, e := range edges {
		if e.Kind == store.EdgeProduces && e.DstName != "topic:orders.created" {
			t.Errorf("in-process event stream produced a messaging edge: %s", e.DstName)
		}
	}
}
