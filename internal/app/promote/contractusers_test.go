package promote

import "testing"

func TestKeyDescribed(t *testing.T) {
	tests := []struct {
		name  string
		query string
		key   string
		want  bool
	}{
		{"rpc method and service named", "which service calls the payment service Charge rpc",
			"grpc:PaymentService/Charge", true},
		{"rpc method named in two words", "what calls the shipping service ShipOrder rpc",
			"grpc:ShippingService/ShipOrder", true},
		{"proto package is not part of the name", "which service calls the payment service Charge rpc",
			"grpc:hipstershop.PaymentService/Charge", true},
		{"sibling rpc of a named service", "what calls the shipping service ShipOrder rpc",
			"grpc:ShippingService/GetQuote", false},
		{"one-word method without its service", "what calls the created-snapshot handler",
			"grpc:AnnotationStore/Create", false},
		{"one-word method with its service", "what calls the annotation store Create rpc",
			"grpc:AnnotationStore/Create", true},
		{"topic described in prose", "which service subscribes to catalog product price changes",
			"topic:ProductPriceChangedIntegrationEvent", true},
		{"topic with one word left over", "which service reacts to the event saying an order's stock was confirmed",
			"topic:OrderStockConfirmedIntegrationEvent", true},
		{"a neighbouring topic", "which service reacts to the event saying an order's stock was confirmed",
			"topic:OrderStatusChangedToStockConfirmedIntegrationEvent", false},
		{"a topic named only by scaffolding", "which service consumes the integration events",
			"topic:IntegrationEvent", false},
		{"unresolved topic reference", "which service consumes the orders topic",
			"topic:${ORDERS_TOPIC}", false},
		{"route with two segments", "which service posts a completed order to the user order history",
			"http:POST /api/user/order/{id}", true},
		{"route with one segment", "what calls the owners endpoint", "http:GET /owners", false},
		{"route parameters are not path words", "which endpoint reads a shopping cart by its id",
			"http:DELETE /cart/{id}", false},
		{"a verb the question did not ask for", "which service posts an order to the user order history",
			"http:GET /api/user/order/{id}", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := keyDescribed(tt.key, questionWords(tt.query), httpVerbsNamed(tt.query))
			if got != tt.want {
				t.Errorf("keyDescribed(%q, %q) = %v, want %v", tt.key, tt.query, got, tt.want)
			}
		})
	}
}

// The key a question describes most completely is the one it means.
func TestKeyDescribedScoresTheFullerMatchHigher(t *testing.T) {
	words := questionWords("what calls the shipping service ShipOrder rpc")
	full, ok := keyDescribed("grpc:ShippingService/ShipOrder", words, nil)
	if !ok {
		t.Fatal("the key the question names was not described")
	}
	partial, ok := keyDescribed("grpc:ShippingService/Ship", words, nil)
	if !ok {
		t.Fatal("a one-word method whose service is named should be described")
	}
	if full <= partial {
		t.Errorf("score for the fully named key = %d, want more than %d", full, partial)
	}
}

func TestStemWord(t *testing.T) {
	// Both sides of the comparison go through this, so what matters is which
	// pairs meet, not what they collapse to.
	same := [][2]string{
		{"changes", "changed"}, {"price", "prices"}, {"orders", "order"},
		{"confirms", "confirmed"}, {"products", "product"}, {"services", "service"},
		{"recommendations", "recommendation"}, {"queries", "query"},
	}
	for _, pair := range same {
		if stemWord(pair[0]) != stemWord(pair[1]) {
			t.Errorf("stemWord(%q) = %q, stemWord(%q) = %q — want the same stem",
				pair[0], stemWord(pair[0]), pair[1], stemWord(pair[1]))
		}
	}
	// Words that mean different things must not be collapsed into one.
	differ := [][2]string{
		{"order", "orchestrator"}, {"address", "addition"}, {"basket", "bask"},
		{"catalog", "cataloguing"}, {"user", "usage"},
	}
	for _, pair := range differ {
		if stemWord(pair[0]) == stemWord(pair[1]) {
			t.Errorf("stemWord(%q) == stemWord(%q) == %q — want different stems",
				pair[0], pair[1], stemWord(pair[0]))
		}
	}
}
