package api

import (
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/graph"
)

// twoReposOneName is the case the node key exists for: both repositories
// contain a service called "api", and each talks to a different backend.
func twoReposOneName() ([]*graph.ServiceInfo, []*graph.ServiceLink) {
	services := []*graph.ServiceInfo{
		{RepoID: "shop", Name: "api"},
		{RepoID: "shop", Name: "orders"},
		{RepoID: "crm", Name: "api"},
		{RepoID: "crm", Name: "leads"},
	}
	links := []*graph.ServiceLink{
		{SrcRepo: "shop", SrcService: "api", DstRepo: "shop", DstService: "orders",
			Kind: "http_call", Via: "http:POST /orders", Count: 1, Confidence: 0.95},
		{SrcRepo: "crm", SrcService: "api", DstRepo: "crm", DstService: "leads",
			Kind: "http_call", Via: "http:POST /leads", Count: 1, Confidence: 0.95},
	}
	return services, links
}

func TestSameNameInTwoReposStaysTwoNodes(t *testing.T) {
	services, links := twoReposOneName()

	for _, tc := range []struct {
		name   string
		render func([]*graph.ServiceInfo, []*graph.ServiceLink) string
		// edge is the substring that proves both edges left distinct nodes.
		arrow string
	}{
		{"mermaid", renderMermaid, "-->"},
		{"dot", renderDOT, "->"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.render(services, links)

			// Four services means four declared nodes, not three.
			for _, id := range []string{"s0", "s1", "s2", "s3"} {
				if !strings.Contains(out, id) {
					t.Errorf("node %s missing — same-named services collapsed:\n%s", id, out)
				}
			}
			if strings.Contains(out, "s4") {
				t.Errorf("more nodes than services:\n%s", out)
			}

			// The two "api" nodes must be labelled apart.
			for _, want := range []string{"shop/api", "crm/api"} {
				if !strings.Contains(out, want) {
					t.Errorf("label %q missing:\n%s", want, out)
				}
			}
			// Unambiguous names stay bare.
			for _, unwanted := range []string{"shop/orders", "crm/leads"} {
				if strings.Contains(out, unwanted) {
					t.Errorf("label %q should not be qualified:\n%s", unwanted, out)
				}
			}

			if n := strings.Count(out, tc.arrow); n != 2 {
				t.Errorf("want 2 edges, got %d:\n%s", n, out)
			}
		})
	}
}

func TestOneRepoKeepsBareLabels(t *testing.T) {
	services := []*graph.ServiceInfo{
		{RepoID: "boutique", Name: "frontend"},
		{RepoID: "boutique", Name: "checkout"},
	}
	links := []*graph.ServiceLink{
		{SrcRepo: "boutique", SrcService: "frontend", DstRepo: "boutique", DstService: "checkout",
			Kind: "rpc_call", Via: "grpc:CheckoutService/PlaceOrder", Count: 1, Confidence: 0.95},
	}

	for _, out := range []string{renderMermaid(services, links), renderDOT(services, links)} {
		if strings.Contains(out, "boutique/") {
			t.Errorf("single repo should not qualify labels:\n%s", out)
		}
		for _, want := range []string{`"frontend"`, `"checkout"`, "gRPC CheckoutService/PlaceOrder"} {
			if !strings.Contains(out, want) {
				t.Errorf("%q missing:\n%s", want, out)
			}
		}
	}
}

// A link endpoint whose file matched no service root falls back to the repo ID
// and has no ServiceInfo, so the label map has to learn about it from the link.
func TestLinkOnlyEndpointGetsANode(t *testing.T) {
	services := []*graph.ServiceInfo{{RepoID: "shop", Name: "api"}}
	links := []*graph.ServiceLink{
		{SrcRepo: "shop", SrcService: "api", DstRepo: "legacy", DstService: "legacy",
			Kind: "http_call", Via: "http:GET /pricing", Count: 3, Confidence: 0.8},
	}

	out := renderMermaid(services, links)
	if !strings.Contains(out, `s1["legacy"]`) {
		t.Errorf("link-only endpoint not declared:\n%s", out)
	}
	if !strings.Contains(out, "s0 -->|GET /pricing| s1") {
		t.Errorf("edge missing or misdirected:\n%s", out)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	services, links := twoReposOneName()
	for i := 0; i < 20; i++ {
		if got := renderMermaid(services, links); got != renderMermaid(services, links) {
			t.Fatalf("mermaid output varies between calls:\n%s", got)
		}
		if got := renderDOT(services, links); got != renderDOT(services, links) {
			t.Fatalf("dot output varies between calls:\n%s", got)
		}
	}
}
