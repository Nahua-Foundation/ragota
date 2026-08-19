package ast

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

func TestPythonExtractor(t *testing.T) {
	src := `import os
import requests

ORDERS_TOPIC = os.getenv("ORDERS_TOPIC")

@app.post("/api/analytics/events")
def ingest_event(user_id: str, amount: float):
    record_event(user_id, amount)

@app.route("/legacy", methods=["POST"])
def legacy_handler():
    pass

def record_event(user_id, amount):
    requests.post("http://audit:9000/api/audit/log", json={"user_id": user_id, "amount": amount})
    db.execute("INSERT INTO analytics_events (user_id, amount) VALUES (?, ?)", user_id, amount)

def start_consumer():
    consumer.subscribe(["orders.created"])

def publish_metrics(user_id):
    producer.send(ORDERS_TOPIC, value={"user_id": user_id})

class EventStore:
    def save(self, event):
        pass
`
	units, edges := parseOrFail(t, "python", "analytics.py", src)

	if findUnit(units, "function", "ingest_event") == nil {
		t.Fatalf("missing function unit; units: %+v", names(units))
	}
	if findUnit(units, store.KindHTTPRoute, "POST /api/analytics/events") == nil {
		t.Errorf("missing fastapi route; units: %+v", names(units))
	}
	if findUnit(units, store.KindHTTPRoute, "POST /legacy") == nil {
		t.Errorf("missing flask route; units: %+v", names(units))
	}
	if findUnit(units, "method", "save") == nil || findUnit(units, "class", "EventStore") == nil {
		t.Errorf("missing class/method units; units: %+v", names(units))
	}
	if findEdge(edges, store.EdgeHandledBy, "ingest_event") == nil {
		t.Errorf("missing handled_by; edges: %+v", edgeNames(edges))
	}

	e := findEdge(edges, store.EdgeHTTPCall, "http:POST /api/audit/log")
	if e == nil {
		t.Fatalf("missing http_call; edges: %+v", edgeNames(edges))
	}
	if m := store.DecodeEdgeMeta(e.Meta); m.Fields["user_id"] != "user_id" {
		t.Errorf("http_call fields = %+v", m.Fields)
	}

	w := findEdge(edges, store.EdgeWritesTo, "db:analytics_events")
	if w == nil {
		t.Fatalf("missing writes_to; edges: %+v", edgeNames(edges))
	}
	if m := store.DecodeEdgeMeta(w.Meta); m.Fields["user_id"] == "" {
		t.Errorf("writes_to fields = %+v", m.Fields)
	}

	if findEdge(edges, store.EdgeConsumes, "topic:orders.created") == nil {
		t.Errorf("missing consumes; edges: %+v", edgeNames(edges))
	}
	// env-driven topic becomes a ${REF} for the linker
	if findEdge(edges, store.EdgeProduces, "topic:${ORDERS_TOPIC}") == nil {
		t.Errorf("missing produces with env ref; edges: %+v", edgeNames(edges))
	}
	if findEdge(edges, store.EdgeCall, "record_event") == nil {
		t.Errorf("missing call edge; edges: %+v", edgeNames(edges))
	}
}

func TestSQLParser(t *testing.T) {
	src := `-- analytics schema
CREATE TABLE IF NOT EXISTS analytics_events (
    id BIGSERIAL PRIMARY KEY,
    user_id TEXT NOT NULL,
    amount NUMERIC(10, 2),
    created_at TIMESTAMP DEFAULT now()
);

ALTER TABLE analytics_events ADD COLUMN source TEXT;
`
	units, _ := parseOrFail(t, "sql", "001_init.sql", src)

	if u := findUnit(units, store.KindDBTable, "analytics_events"); u == nil {
		t.Fatalf("missing table unit; units: %+v", names(units))
	} else if u.Qualified != "db:analytics_events" {
		t.Errorf("table qualified = %q", u.Qualified)
	}
	if u := findUnit(units, store.KindDBColumn, "user_id"); u == nil {
		t.Errorf("missing column unit; units: %+v", names(units))
	} else if u.Qualified != "db:analytics_events.user_id" {
		t.Errorf("column qualified = %q", u.Qualified)
	}
	if findUnit(units, store.KindDBColumn, "source") == nil {
		t.Errorf("missing ALTER-added column; units: %+v", names(units))
	}
	// constraint lines must not become columns
	if findUnit(units, store.KindDBColumn, "primary") != nil {
		t.Errorf("constraint parsed as column")
	}
}

func TestGoSQLDetection(t *testing.T) {
	src := `package main

func saveOrder(userID string, amount float64) {
	db.Exec("INSERT INTO orders (user_id, amount) VALUES (?, ?)", userID, amount)
}

func loadOrders(userID string) {
	db.Query("SELECT id, amount FROM orders WHERE user_id = ?", userID)
}
`
	_, edges := parseOrFail(t, "go", "db.go", src)

	w := findEdge(edges, store.EdgeWritesTo, "db:orders")
	if w == nil {
		t.Fatalf("missing writes_to; edges: %+v", edgeNames(edges))
	}
	if m := store.DecodeEdgeMeta(w.Meta); m.Fields["user_id"] == "" {
		t.Errorf("writes_to fields = %+v", m.Fields)
	}
	if findEdge(edges, store.EdgeReadsFrom, "db:orders") == nil {
		t.Errorf("missing reads_from; edges: %+v", edgeNames(edges))
	}
}

func TestConfigParserYAML(t *testing.T) {
	src := `kafka:
  brokers: kafka:9092
  orders-topic: orders.created
server:
  port: 8080
`
	units, _ := parseOrFail(t, "yaml", "application.yaml", src)

	var found *domain.ASTUnit
	for _, u := range units {
		if u.Qualified == "config:kafka.orders-topic" {
			found = u
		}
	}
	if found == nil {
		t.Fatalf("missing kafka.orders-topic key; units: %+v", names(units))
	}
	if found.Signature != "orders.created" {
		t.Errorf("value = %q", found.Signature)
	}
}

func TestConfigParserProperties(t *testing.T) {
	src := "# env\nORDERS_TOPIC=orders.created\nDB_URL=postgres://x\n"
	units, _ := parseOrFail(t, "properties", ".env", src)

	var found *domain.ASTUnit
	for _, u := range units {
		if u.Qualified == "config:ORDERS_TOPIC" {
			found = u
		}
	}
	if found == nil || found.Signature != "orders.created" {
		t.Fatalf("ORDERS_TOPIC not parsed; units: %+v", names(units))
	}
}

func TestOpenAPIImport(t *testing.T) {
	src := `openapi: "3.0.0"
info:
  title: Payments API
paths:
  /api/payments:
    post:
      summary: Create a payment
    get:
      summary: List payments
  /api/payments/{id}:
    get:
      summary: Get payment
`
	units, _ := parseOrFail(t, "yaml", "openapi.yaml", src)

	if u := findUnit(units, store.KindHTTPRoute, "POST /api/payments"); u == nil {
		t.Fatalf("missing POST route from openapi; units: %+v", names(units))
	} else if u.Doc != "Create a payment" {
		t.Errorf("route doc = %q", u.Doc)
	}
	if findUnit(units, store.KindHTTPRoute, "GET /api/payments/{id}") == nil {
		t.Errorf("missing templated GET route; units: %+v", names(units))
	}
	// OpenAPI docs must not leak config keys
	for _, u := range units {
		if u.Kind == store.KindConfigKey {
			t.Errorf("unexpected config key %q from openapi doc", u.Qualified)
		}
	}
}

func TestGoEnvTopicRef(t *testing.T) {
	src := `package main

import "os"

func publish() {
	topic := os.Getenv("ORDERS_TOPIC")
	w := &kafka.Writer{Topic: topic}
	w.WriteMessages(ctx, msg)
}
`
	_, edges := parseOrFail(t, "go", "envkafka.go", src)
	if findEdge(edges, store.EdgeProduces, "topic:${ORDERS_TOPIC}") == nil {
		t.Fatalf("missing produces with env ref; edges: %+v", edgeNames(edges))
	}
}
