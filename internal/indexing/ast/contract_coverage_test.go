package ast

import (
	"context"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// The counters behind GET /repos/{id}/coverage: per contract kind, how many
// call sites looked like an outbound contract and how many produced an edge.

func TestFileCoverageCounters(t *testing.T) {
	tests := []struct {
		name string
		lang string
		file string
		src  string
		want map[string]storage.CoverageCounts
	}{
		{
			name: "an http call site that resolves and one that does not",
			lang: "go", file: "client.go",
			src: `package client

func fetch(url string) {
	http.Get("http://users/api/users")
	http.Get(url)
}
`,
			want: map[string]storage.CoverageCounts{
				storage.ContractKindHTTP:      {Candidates: 2, Edges: 1},
				storage.ContractKindRPC:       {},
				storage.ContractKindMessaging: {},
				storage.ContractKindDB:        {},
			},
		},
		{
			name: "a publish whose topic is a variable is a candidate",
			lang: "python", file: "producer.py",
			src: `def publish(topic, payload):
    producer.send("orders.created", payload)
    producer.send(topic, payload)
`,
			want: map[string]storage.CoverageCounts{
				storage.ContractKindHTTP:      {},
				storage.ContractKindRPC:       {},
				storage.ContractKindMessaging: {Candidates: 2, Edges: 1},
				storage.ContractKindDB:        {},
			},
		},
		{
			name: "a subscription naming several topics is one call site",
			lang: "typescript", file: "consumer.ts",
			src: `async function run() {
  await consumer.subscribe({ topics: ['orders.created', 'orders.paid'] });
}
`,
			want: map[string]storage.CoverageCounts{
				storage.ContractKindHTTP:      {},
				storage.ContractKindRPC:       {},
				storage.ContractKindMessaging: {Candidates: 1, Edges: 1},
				storage.ContractKindDB:        {},
			},
		},
		{
			name: "sql in a string argument, with and without a parsable table",
			lang: "go", file: "store.go",
			src: `package store

func load(db *sql.DB, q string) {
	db.Query("SELECT id FROM orders WHERE id = $1")
	db.Query("SELECT " + q)
}
`,
			want: map[string]storage.CoverageCounts{
				storage.ContractKindHTTP:      {},
				storage.ContractKindRPC:       {},
				storage.ContractKindMessaging: {},
				storage.ContractKindDB:        {Candidates: 1, Edges: 1},
			},
		},
		{
			name: "an amqp publish and consume are messaging call sites",
			lang: "python", file: "broker.py",
			src: `def wire(channel, exchange, routing_key):
    channel.basic_publish(exchange='events', routing_key='orders', body=b'')
    channel.basic_publish(exchange=exchange, routing_key=routing_key, body=b'')
    channel.basic_consume(queue='orders', on_message_callback=handle)
`,
			want: map[string]storage.CoverageCounts{
				storage.ContractKindHTTP:      {},
				storage.ContractKindRPC:       {},
				storage.ContractKindMessaging: {Candidates: 3, Edges: 2},
				storage.ContractKindDB:        {},
			},
		},
		{
			name: "an event-typed publish is a messaging call site, its abstract base is not",
			lang: "csharp", file: "Publisher.cs",
			src: `namespace App;

public class Publisher(IEventBus eventBus)
{
    public async Task Send(int id)
    {
        var evt = new OrderPaidIntegrationEvent(id);
        await eventBus.PublishAsync(evt);
    }
}
`,
			want: map[string]storage.CoverageCounts{
				storage.ContractKindHTTP:      {},
				storage.ContractKindRPC:       {},
				storage.ContractKindMessaging: {Candidates: 1, Edges: 1},
				storage.ContractKindDB:        {},
			},
		},
		{
			name: "an ef core query through a context is a db call site",
			lang: "csharp", file: "Queries.cs",
			src: `namespace App;

public class Queries
{
    private readonly OrderingContext _context;

    public async Task<Order> Get(int id)
    {
        return await _context.Orders.FindAsync(id);
    }
}
`,
			want: map[string]storage.CoverageCounts{
				storage.ContractKindHTTP:      {},
				storage.ContractKindRPC:       {},
				storage.ContractKindMessaging: {},
				storage.ContractKindDB:        {Candidates: 1, Edges: 1},
			},
		},
		{
			name: "an orm repository access is a db call site",
			lang: "typescript", file: "orders.service.ts",
			src: `export class OrderService {
  constructor(private readonly orderRepository: Repository<Order>) {}

  async run(dto: any) {
    await this.orderRepository.save(dto);
    return this.orderRepository.find();
  }
}
`,
			want: map[string]storage.CoverageCounts{
				storage.ContractKindHTTP:      {},
				storage.ContractKindRPC:       {},
				storage.ContractKindMessaging: {},
				storage.ContractKindDB:        {Candidates: 2, Edges: 2},
			},
		},
		{
			name: "an express response is not a missed messaging contract",
			lang: "javascript", file: "server.js",
			src: `function handler(req, res) {
  res.status(500).send('database not available');
  res.send('ok');
}
`,
			want: map[string]storage.CoverageCounts{
				storage.ContractKindHTTP:      {},
				storage.ContractKindRPC:       {},
				storage.ContractKindMessaging: {},
				storage.ContractKindDB:        {},
			},
		},
		{
			name: "a file with nothing to find reports zeroes, not silence",
			lang: "go", file: "math.go",
			src: `package math

func Add(a, b int) int { return a + b }
`,
			want: map[string]storage.CoverageCounts{
				storage.ContractKindHTTP:      {},
				storage.ContractKindRPC:       {},
				storage.ContractKindMessaging: {},
				storage.ContractKindDB:        {},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := parseFactsOrFail(t, tt.lang, tt.file, tt.src)
			if len(facts.Coverage) != len(storage.ContractKinds) {
				t.Fatalf("coverage = %v, want every kind reported", facts.Coverage)
			}
			for kind, want := range tt.want {
				if got := facts.Coverage[kind]; got != want {
					t.Errorf("coverage[%s] = %+v, want %+v", kind, got, want)
				}
			}
			for kind, c := range facts.Coverage {
				if c.Edges > c.Candidates {
					t.Errorf("coverage[%s] = %+v: every edge starts as a candidate", kind, c)
				}
			}
		})
	}
}

// A parser that does not look for contracts reports nothing, which the report
// must be able to tell apart from a zero.
func TestCoverageAbsentForNonCallLanguages(t *testing.T) {
	for _, lang := range []string{"sql", "proto", "yaml"} {
		if _, ok := GetParserForLanguage(lang).(FactsParser); ok {
			t.Errorf("%s parser reports coverage it does not collect", lang)
		}
	}
}

func TestIndexResultContractCoverage(t *testing.T) {
	files := []*indexing.FileToIndex{
		{
			Path: "svc/a.go", Language: "go",
			Content: []byte(`package svc

func a(url string) {
	http.Get("http://users/api/users")
	http.Post(url, "application/json", body)
}
`),
		},
		{
			Path: "svc/b.go", Language: "go",
			Content: []byte(`package svc

func b() {
	http.Get("http://orders/api/orders")
}
`),
		},
	}

	mem := &memStorage{}
	idx := New(&Config{Storage: mem})
	RegisterDefaultParsers(idx)
	result, err := idx.Index(context.Background(), &indexing.IndexRequest{RepoID: "r", Files: files})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	var reporter interface {
		ContractCoverage() map[string]storage.CoverageCounts
	} = result
	cov := reporter.ContractCoverage()

	// Summed over the files of this run, and this run only: the service adds
	// the batches up itself.
	if got, want := cov[storage.ContractKindHTTP], (storage.CoverageCounts{Candidates: 3, Edges: 2}); got != want {
		t.Errorf("http coverage = %+v, want %+v", got, want)
	}
	for _, kind := range storage.ContractKinds {
		if _, ok := cov[kind]; !ok {
			t.Errorf("kind %s missing from %v", kind, cov)
		}
	}

	// Re-indexing the same batch reports the same counters, never the sum.
	again, err := idx.Index(context.Background(), &indexing.IndexRequest{RepoID: "r", Files: files})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if got := again.ContractCoverage()[storage.ContractKindHTTP]; got != cov[storage.ContractKindHTTP] {
		t.Errorf("second run http coverage = %+v, want %+v", got, cov[storage.ContractKindHTTP])
	}
}

// A call site the wrapper explained is a candidate that produced an edge.
func TestCoverageCountsWrapperAttributedCalls(t *testing.T) {
	src := `package client

func postCharge(payload []byte) {
	http.Post("http://billing/api/billing/charge", "application/json", payload)
}

func Charge(o *Order) { postCharge(o.Payload) }
`
	facts := parseFactsOrFail(t, "go", "billing/client.go", src)
	want := storage.CoverageCounts{Candidates: 2, Edges: 2}
	if got := facts.Coverage[storage.ContractKindHTTP]; got != want {
		t.Errorf("http coverage = %+v, want %+v", got, want)
	}
}

// A helper whose route is fixed and whose name is an HTTP method is not
// followed: the name is what the attribution joins on, and every mapping in
// the language has a get.
func TestWrapperNotFollowedWhenNamedAfterAMethod(t *testing.T) {
	src := `package client

func get(id string) {
	http.Get("http://billing/api/billing/charge")
}

func Charge(o *Order) {
	get(o.ID)
	defaults.get("timeout")
}
`
	facts := parseFactsOrFail(t, "go", "billing/client.go", src)
	for _, e := range facts.Edges {
		if e.Kind == storage.EdgeHTTPCall && storage.DecodeEdgeMeta(e.Meta).Source != "" {
			t.Errorf("wrapper-attributed edge %q at line %d, want none", e.DstName, e.Line)
		}
	}
	want := storage.CoverageCounts{Candidates: 1, Edges: 1}
	if got := facts.Coverage[storage.ContractKindHTTP]; got != want {
		t.Errorf("http coverage = %+v, want %+v", got, want)
	}
}
