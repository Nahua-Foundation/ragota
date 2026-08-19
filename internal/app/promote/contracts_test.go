package promote

import "testing"

func TestContractKeys(t *testing.T) {
	tests := []struct {
		query string
		want  []string
	}{
		// A path parameter reduces to "{}" whatever syntax wrote it, so the
		// question and the route it names produce one key (contract.HTTP).
		{"where does POST /admin/draft-orders/:id/convert-to-order go",
			[]string{"http:POST /admin/draft-orders/{}/convert-to-order"}},
		{"which handler serves get /api/v1/pets/",
			[]string{"http:GET /api/v1/pets"}}, // trailing slash and case normalized
		{"a route with a query string GET /api/orders?status=new",
			[]string{"http:GET /api/orders"}}, // query string is per-request, not part of the key
		{"where do rows get inserted into the login_attempt table",
			[]string{"db:login_attempt"}},
		{"which model maps to the database table named image",
			[]string{"db:image"}},
		{"which service consumes the orders queue",
			[]string{"topic:orders"}},
		{"who publishes to topic orders.created",
			[]string{"topic:orders.created"}},
		{"where is CustomersService/GetOwner implemented",
			[]string{"grpc:CustomersService/GetOwner"}},
		{"where is the grpc method AddItem implemented",
			[]string{"grpc:/AddItem"}},
	}
	for _, tt := range tests {
		var got []string
		for _, ref := range contractKeys(tt.query) {
			got = append(got, ref.key)
		}
		for _, want := range tt.want {
			if !containsString(got, want) {
				t.Errorf("contractKeys(%q) = %v, want it to include %q", tt.query, got, want)
			}
		}
	}

	// A question about the concept, not about a named entity, has no key: the
	// grammar words around "table"/"queue" must never become one.
	for _, q := range []string{
		"how does the service write to the database table",
		"which endpoint returns the list of veterinarians",
		"where is the message queue configured",
		"how does retry work",
	} {
		if refs := contractKeys(q); len(refs) > 0 {
			var got []string
			for _, r := range refs {
				got = append(got, r.key)
			}
			t.Errorf("contractKeys(%q) = %v, want none", q, got)
		}
	}
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
