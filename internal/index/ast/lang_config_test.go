package ast

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/store"
)

func TestAsyncAPIv2Channels(t *testing.T) {
	src := `asyncapi: "2.6.0"
info:
  title: Orders Events
channels:
  orders.created:
    description: Order lifecycle events
    publish:
      message:
        name: OrderCreated
    subscribe:
      message:
        name: OrderCreated
  payments.settled:
    subscribe:
      message:
        name: PaymentSettled
`
	units, _ := parseOrFail(t, "yaml", "asyncapi.yaml", src)

	if len(units) != 2 {
		t.Fatalf("got %d units, want 2; units: %+v", len(units), names(units))
	}
	// Sorted by name for determinism.
	if units[0].Name != "orders.created" || units[1].Name != "payments.settled" {
		t.Errorf("unit order = %v, %v", units[0].Name, units[1].Name)
	}

	oc := findUnit(units, store.KindTopicChannel, "orders.created")
	if oc == nil {
		t.Fatalf("missing orders.created channel; units: %+v", names(units))
	}
	if oc.Qualified != "topic:orders.created" {
		t.Errorf("qualified = %q, want topic:orders.created", oc.Qualified)
	}
	if oc.Doc != "Order lifecycle events" {
		t.Errorf("doc = %q", oc.Doc)
	}
	if oc.Signature != "ops:publish,subscribe" {
		t.Errorf("signature = %q, want ops:publish,subscribe", oc.Signature)
	}
	if oc.StartLine != 1 || oc.EndLine != 1 || oc.Hash == "" {
		t.Errorf("lines/hash = %d/%d/%q", oc.StartLine, oc.EndLine, oc.Hash)
	}

	ps := findUnit(units, store.KindTopicChannel, "payments.settled")
	if ps == nil {
		t.Fatalf("missing payments.settled channel; units: %+v", names(units))
	}
	if ps.Signature != "ops:subscribe" {
		t.Errorf("signature = %q, want ops:subscribe", ps.Signature)
	}
}

func TestAsyncAPIv3ChannelAddress(t *testing.T) {
	src := `asyncapi: "3.0.0"
info:
  title: Orders Events
channels:
  ordersCreated:
    address: orders.created
    description: Order lifecycle events
  legacyChannel:
    description: No address, key is the topic
`
	units, _ := parseOrFail(t, "yaml", "asyncapi.yaml", src)

	if len(units) != 2 {
		t.Fatalf("got %d units, want 2; units: %+v", len(units), names(units))
	}
	oc := findUnit(units, store.KindTopicChannel, "orders.created")
	if oc == nil {
		t.Fatalf("address not used as topic name; units: %+v", names(units))
	}
	if oc.Qualified != "topic:orders.created" || oc.Doc != "Order lifecycle events" {
		t.Errorf("qualified/doc = %q/%q", oc.Qualified, oc.Doc)
	}
	if lc := findUnit(units, store.KindTopicChannel, "legacyChannel"); lc == nil {
		t.Errorf("channel key fallback missing; units: %+v", names(units))
	}
}

func TestAsyncAPINegative(t *testing.T) {
	// A plain yaml config must not produce topic_channel units.
	plain := `kafka:
  brokers: kafka:9092
channels:
  orders.created:
    description: looks like a channel but no asyncapi marker
`
	units, _ := parseOrFail(t, "yaml", "application.yaml", plain)
	for _, u := range units {
		if u.Kind == store.KindTopicChannel {
			t.Errorf("unexpected topic_channel unit %q in plain yaml", u.Name)
		}
	}

	// An AsyncAPI doc must not scatter into config_key units.
	spec := `asyncapi: "2.6.0"
info:
  title: Orders Events
channels:
  orders.created:
    publish:
      message:
        name: OrderCreated
`
	units, _ = parseOrFail(t, "yaml", "asyncapi.yaml", spec)
	for _, u := range units {
		if u.Kind == store.KindConfigKey {
			t.Errorf("unexpected config_key unit %q in asyncapi doc", u.Qualified)
		}
	}
	if findUnit(units, store.KindTopicChannel, "orders.created") == nil {
		t.Errorf("missing orders.created channel; units: %+v", names(units))
	}
}
