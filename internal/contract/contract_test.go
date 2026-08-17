package contract

import "testing"

func TestGRPC(t *testing.T) {
	tests := []struct {
		name            string
		service, method string
		want            string
	}{
		{"with package", "orders.OrderService", "CreateOrder", "grpc:orders.OrderService/CreateOrder"},
		{"without package", "OrderService", "CreateOrder", "grpc:OrderService/CreateOrder"},
		{"empty service", "", "CreateOrder", "grpc:/CreateOrder"},
		{"empty method", "Svc", "", "grpc:Svc/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GRPC(tt.service, tt.method); got != tt.want {
				t.Errorf("GRPC(%q, %q) = %q, want %q", tt.service, tt.method, got, tt.want)
			}
		})
	}
}

func TestHTTP(t *testing.T) {
	tests := []struct {
		name         string
		method, path string
		want         string
	}{
		{"plain", "GET", "/api/users", "http:GET /api/users"},
		{"lowercase method", "post", "/orders", "http:POST /orders"},
		{"empty method is ANY", "", "/health", "http:ANY /health"},
		{"whitespace method", "  get ", "/x", "http:GET /x"},
		{"no leading slash", "GET", "users", "http:GET /users"},
		{"trailing slash trimmed", "GET", "/users/", "http:GET /users"},
		{"both slashes trimmed", "PUT", "users/42/", "http:PUT /users/42"},
		{"root path", "GET", "/", "http:GET /"},
		{"empty path", "GET", "", "http:GET /"},
		{"whitespace path", "GET", "  /a/b  ", "http:GET /a/b"},
		// Query strings and fragments are request data, not part of the route.
		{"query string dropped", "GET", "/api/orders?status=new", "http:GET /api/orders"},
		{"query string with several params", "GET", "/api/orders?a=1&b=2", "http:GET /api/orders"},
		{"fragment dropped", "GET", "/api/orders#top", "http:GET /api/orders"},
		{"query before trailing slash", "GET", "/api/orders/?x=1", "http:GET /api/orders"},
		{"path is only a query", "GET", "?x=1", "http:GET /"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HTTP(tt.method, tt.path); got != tt.want {
				t.Errorf("HTTP(%q, %q) = %q, want %q", tt.method, tt.path, got, tt.want)
			}
		})
	}
}

func TestSimpleConstructors(t *testing.T) {
	if got := Topic("orders.created"); got != "topic:orders.created" {
		t.Errorf("Topic = %q", got)
	}
	if got := TopicRef("ORDERS_TOPIC"); got != "topic:${ORDERS_TOPIC}" {
		t.Errorf("TopicRef = %q", got)
	}
	if got := DB("orders"); got != "db:orders" {
		t.Errorf("DB = %q", got)
	}
	if got := Config("kafka.orders-topic"); got != "config:kafka.orders-topic" {
		t.Errorf("Config = %q", got)
	}
}

// TestParseGRPC mirrors the historical splitGrpcKey behavior bit-for-bit,
// including the liberal forms and partial results on failure.
func TestParseGRPC(t *testing.T) {
	tests := []struct {
		in              string
		service, method string
		ok              bool
	}{
		{"grpc:pkg.Svc/Method", "pkg.Svc", "Method", true},
		{"grpc:Svc/Method", "Svc", "Method", true},
		{"grpc:/Method", "", "Method", true},
		{"grpc:Method", "", "Method", true},
		{"grpc:Svc/", "Svc", "", false},
		{"grpc:", "", "", false},
		{"http:GET /x", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		service, method, ok := ParseGRPC(tt.in)
		if service != tt.service || method != tt.method || ok != tt.ok {
			t.Errorf("ParseGRPC(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.in, service, method, ok, tt.service, tt.method, tt.ok)
		}
	}
}

func TestParseHTTP(t *testing.T) {
	tests := []struct {
		in           string
		method, path string
		ok           bool
	}{
		{"http:GET /a/b", "GET", "/a/b", true},
		{"http:ANY /", "ANY", "/", true},
		{"http:GETnospace", "", "", false},
		{"grpc:Svc/M", "", "", false},
		{"http:", "", "", false},
		{"", "", "", false},
		// Keys stored before HTTP normalized them still join.
		{"http:GET /a/b?x=1", "GET", "/a/b", true},
		{"http:GET /a/b#frag", "GET", "/a/b", true},
	}
	for _, tt := range tests {
		method, path, ok := ParseHTTP(tt.in)
		if method != tt.method || path != tt.path || ok != tt.ok {
			t.Errorf("ParseHTTP(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.in, method, path, ok, tt.method, tt.path, tt.ok)
		}
	}
}

func TestParseTopic(t *testing.T) {
	tests := []struct {
		in   string
		name string
		ok   bool
	}{
		{"topic:orders.created", "orders.created", true},
		{"topic:", "", true},
		{"topic:${REF}", "${REF}", true}, // references parse as literal names
		{"db:orders", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		name, ok := ParseTopic(tt.in)
		if name != tt.name || ok != tt.ok {
			t.Errorf("ParseTopic(%q) = (%q, %v), want (%q, %v)", tt.in, name, ok, tt.name, tt.ok)
		}
	}
}

func TestParseTopicRef(t *testing.T) {
	tests := []struct {
		in  string
		ref string
		ok  bool
	}{
		{"topic:${ORDERS_TOPIC}", "ORDERS_TOPIC", true},
		{"topic:${}", "", true},
		{"topic:${unclosed", "", false},
		{"topic:orders.created", "", false},
		{"topic:$", "", false},
		{"db:${X}", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		ref, ok := ParseTopicRef(tt.in)
		if ref != tt.ref || ok != tt.ok {
			t.Errorf("ParseTopicRef(%q) = (%q, %v), want (%q, %v)", tt.in, ref, ok, tt.ref, tt.ok)
		}
	}
}

func TestParseDBAndConfig(t *testing.T) {
	if table, ok := ParseDB("db:orders"); table != "orders" || !ok {
		t.Errorf("ParseDB(db:orders) = (%q, %v)", table, ok)
	}
	if _, ok := ParseDB("orders"); ok {
		t.Error("ParseDB without prefix should not be ok")
	}
	if path, ok := ParseConfig("config:kafka.topic"); path != "kafka.topic" || !ok {
		t.Errorf("ParseConfig = (%q, %v)", path, ok)
	}
	if _, ok := ParseConfig("kafka.topic"); ok {
		t.Error("ParseConfig without prefix should not be ok")
	}
}

func TestIsKindAndTrimKind(t *testing.T) {
	tests := []struct {
		key  string
		kind Kind
		is   bool
		trim string
	}{
		{"grpc:Svc/M", KindGRPC, true, "Svc/M"},
		{"grpc:Svc/M", KindHTTP, false, "grpc:Svc/M"},
		{"http:GET /x", KindHTTP, true, "GET /x"},
		{"topic:orders", KindTopic, true, "orders"},
		{"db:orders", KindDB, true, "orders"},
		{"db:", KindDB, true, ""},
		{"config:a.b", KindConfig, true, "a.b"},
		{"orders", KindTopic, false, "orders"},
		{"", KindDB, false, ""},
	}
	for _, tt := range tests {
		if got := IsKind(tt.key, tt.kind); got != tt.is {
			t.Errorf("IsKind(%q, %q) = %v, want %v", tt.key, tt.kind, got, tt.is)
		}
		if got := TrimKind(tt.key, tt.kind); got != tt.trim {
			t.Errorf("TrimKind(%q, %q) = %q, want %q", tt.key, tt.kind, got, tt.trim)
		}
	}
}

func TestRoundtrip(t *testing.T) {
	t.Run("grpc", func(t *testing.T) {
		tests := []struct{ service, method string }{
			{"orders.OrderService", "CreateOrder"},
			{"Svc", "M"},
			{"", "Method"}, // empty service survives the roundtrip
		}
		for _, tt := range tests {
			key := GRPC(tt.service, tt.method)
			service, method, ok := ParseGRPC(key)
			if !ok || service != tt.service || method != tt.method {
				t.Errorf("ParseGRPC(GRPC(%q, %q)) = (%q, %q, %v)",
					tt.service, tt.method, service, method, ok)
			}
			if !IsKind(key, KindGRPC) {
				t.Errorf("IsKind(%q, grpc) = false", key)
			}
		}
	})

	t.Run("http", func(t *testing.T) {
		// Normalized inputs roundtrip exactly.
		tests := []struct{ method, path string }{
			{"GET", "/api/users"},
			{"ANY", "/"},
			{"POST", "/orders/{}"}, // already reduced: a parameter is "{}"
		}
		for _, tt := range tests {
			key := HTTP(tt.method, tt.path)
			method, path, ok := ParseHTTP(key)
			if !ok || method != tt.method || path != tt.path {
				t.Errorf("ParseHTTP(HTTP(%q, %q)) = (%q, %q, %v)",
					tt.method, tt.path, method, path, ok)
			}
		}
		// Non-normalized input roundtrips to its normalized form.
		method, path, ok := ParseHTTP(HTTP("get", "users/"))
		if !ok || method != "GET" || path != "/users" {
			t.Errorf("ParseHTTP(HTTP(get, users/)) = (%q, %q, %v)", method, path, ok)
		}
	})

	t.Run("topic", func(t *testing.T) {
		if name, ok := ParseTopic(Topic("orders.created")); !ok || name != "orders.created" {
			t.Errorf("topic roundtrip = (%q, %v)", name, ok)
		}
		if ref, ok := ParseTopicRef(TopicRef("ORDERS_TOPIC")); !ok || ref != "ORDERS_TOPIC" {
			t.Errorf("topic ref roundtrip = (%q, %v)", ref, ok)
		}
	})

	t.Run("db and config", func(t *testing.T) {
		if table, ok := ParseDB(DB("orders")); !ok || table != "orders" {
			t.Errorf("db roundtrip = (%q, %v)", table, ok)
		}
		if path, ok := ParseConfig(Config("kafka.orders-topic")); !ok || path != "kafka.orders-topic" {
			t.Errorf("config roundtrip = (%q, %v)", path, ok)
		}
	})
}

// TestConfidenceValues freezes the numeric tiers: they are persisted in
// storage and multiplied into stored edge confidences.
func TestConfidenceValues(t *testing.T) {
	tests := []struct {
		name string
		got  float32
		want float32
	}{
		{"ConfExact", ConfExact, 0.95},
		{"ConfHigh", ConfHigh, 0.9},
		{"ConfCrossFile", ConfCrossFile, 0.8},
		{"ConfHeuristic", ConfHeuristic, 0.7},
		{"ConfWeak", ConfWeak, 0.6},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

// TestHTTPReducesPathParameters: the same route written by two frameworks has
// to produce one key, or the two sides of a contract never join on it. This
// was a real failure — an Express route declared "/check/:id" while its
// caller's URL was extracted as "/check/{id}".
func TestHTTPReducesPathParameters(t *testing.T) {
	same := []string{
		"/check/:id",         // Express, Rails, gin
		"/check/{id}",        // Spring, ASP.NET, chi, OpenAPI
		"/check/{id:[0-9]+}", // gorilla/mux with a pattern
		"/check/<id>",        // Flask
		"/check/<int:id>",    // Flask with a converter
		"/check/[id]",        // Next.js
		"/check/{differentName}",
	}
	want := HTTP("GET", same[0])
	if want != "http:GET /check/{}" {
		t.Fatalf("HTTP(GET, %q) = %q, want http:GET /check/{}", same[0], want)
	}
	for _, path := range same[1:] {
		if got := HTTP("GET", path); got != want {
			t.Errorf("HTTP(GET, %q) = %q, want %q", path, got, want)
		}
	}

	// Literal text is never touched — only the parameters inside it.
	for path, expect := range map[string]string{
		"/api/v1.0/users":        "http:GET /api/v1.0/users",
		"/files/report.{ext}":    "http:GET /files/report.{}",
		"/users/{id}N{tenant}/x": "http:GET /users/{}N{}/x",
		"/a/{id}/b/:name/c":      "http:GET /a/{}/b/{}/c",
		"/static/*":              "http:GET /static/{}",
		"/":                      "http:GET /",
	} {
		if got := HTTP("GET", path); got != expect {
			t.Errorf("HTTP(GET, %q) = %q, want %q", path, got, expect)
		}
	}
}

func TestTableName(t *testing.T) {
	tests := []struct {
		entity string
		want   string
		note   string
	}{
		// The common case: one derivation both sides already agreed on.
		{"User", "users", ""},
		{"OrderItem", "order_items", ""},
		{"City", "cities", ""},
		{"Box", "boxes", ""},
		{"Dish", "dishes", ""},
		{"HTTPServer", "http_servers", ""},
		{"APIKey", "api_keys", ""},
		{"user_profile", "user_profiles", ""},
		{"orderItem", "order_items", ""},
		{"*User", "users", "pointer receivers reach the extractor"},
		{" User ", "users", ""},

		// The cases the two derivations used to disagree on, which is the point
		// of having one. Each was a silent un-join: the indexer stored one key
		// and the linker looked up another.
		{"User_Profile", "user_profiles", "was stored user__profiles, looked up user_profiles"},
		{"models.User", "users", "was stored users, looked up models_users"},
		{"pkg.models.OrderItem", "order_items", "package qualifiers are not part of the table name"},
		{"user-profile", "user_profiles", "was stored user-profiles, looked up user_profiles"},

		{"", "", "nothing in, nothing out"},
	}
	for _, tt := range tests {
		if got := TableName(tt.entity); got != tt.want {
			t.Errorf("TableName(%q) = %q, want %q %s", tt.entity, got, tt.want, tt.note)
		}
	}
}
