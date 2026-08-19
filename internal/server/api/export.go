package api

import (
	"fmt"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/graph"
)

// nodeKey identifies a service node by repository and name. Service names are
// only made unique within a repository (see svcdetect.disambiguateNames), so
// keying a diagram on the name alone merges two repositories' "api" or
// "frontend" into one node carrying the edges of both.
func nodeKey(repo, service string) string { return repo + "\x00" + service }

// serviceLabels assigns each node its display text: the bare service name where
// it is unambiguous, qualified with the repository where several repositories
// share it. Link endpoints are collected alongside the detected services
// because an endpoint whose file matches no service root falls back to the
// repo ID and has no ServiceInfo of its own.
func serviceLabels(services []*graph.ServiceInfo, links []*graph.ServiceLink) map[string]string {
	holders := map[string]map[string]bool{} // service name -> repos containing it
	note := func(repo, service string) {
		if holders[service] == nil {
			holders[service] = map[string]bool{}
		}
		holders[service][repo] = true
	}
	for _, s := range services {
		note(s.RepoID, s.Name)
	}
	for _, l := range links {
		note(l.SrcRepo, l.SrcService)
		note(l.DstRepo, l.DstService)
	}

	labels := map[string]string{}
	for service, repos := range holders {
		for repo := range repos {
			label := service
			if len(repos) > 1 {
				label = repo + "/" + service
			}
			labels[nodeKey(repo, service)] = label
		}
	}
	return labels
}

// nodeNamer hands out one stable diagram id per (repo, service), declaring the
// node on first use through declare.
func nodeNamer(labels map[string]string, declare func(id, label string)) func(repo, service string) string {
	ids := map[string]string{}
	return func(repo, service string) string {
		key := nodeKey(repo, service)
		if v, ok := ids[key]; ok {
			return v
		}
		v := fmt.Sprintf("s%d", len(ids))
		ids[key] = v
		declare(v, labels[key])
		return v
	}
}

// renderMermaid renders the service graph as a Mermaid flowchart.
func renderMermaid(services []*graph.ServiceInfo, links []*graph.ServiceLink) string {
	var b strings.Builder
	b.WriteString("flowchart LR\n")

	id := nodeNamer(serviceLabels(services, links), func(id, label string) {
		fmt.Fprintf(&b, "    %s[%q]\n", id, label)
	})

	for _, s := range services {
		id(s.RepoID, s.Name)
	}
	for _, l := range links {
		fmt.Fprintf(&b, "    %s -->|%s| %s\n",
			id(l.SrcRepo, l.SrcService), linkLabel(l), id(l.DstRepo, l.DstService))
	}
	return b.String()
}

// renderDOT renders the service graph in Graphviz DOT format. Nodes are
// declared by generated id with the name in a label attribute; using the name
// as the DOT id would merge same-named services the way Mermaid did.
func renderDOT(services []*graph.ServiceInfo, links []*graph.ServiceLink) string {
	var b strings.Builder
	b.WriteString("digraph services {\n    rankdir=LR;\n    node [shape=box];\n")

	id := nodeNamer(serviceLabels(services, links), func(id, label string) {
		fmt.Fprintf(&b, "    %s [label=%q];\n", id, label)
	})

	for _, s := range services {
		id(s.RepoID, s.Name)
	}
	for _, l := range links {
		fmt.Fprintf(&b, "    %s -> %s [label=%q];\n",
			id(l.SrcRepo, l.SrcService), id(l.DstRepo, l.DstService), linkLabel(l))
	}
	b.WriteString("}\n")
	return b.String()
}

func linkLabel(l *graph.ServiceLink) string {
	via := l.Via
	switch l.Kind {
	case "kafka_flow":
		via = "kafka " + contract.TrimKind(via, contract.KindTopic)
	case "rpc_call":
		via = "gRPC " + contract.TrimKind(via, contract.KindGRPC)
	case "http_call":
		via = contract.TrimKind(via, contract.KindHTTP)
	case "runtime_call":
		via = "runtime"
	}
	// Mermaid edge labels break on quotes/pipes.
	via = strings.NewReplacer("\"", "", "|", "/").Replace(via)
	return via
}
