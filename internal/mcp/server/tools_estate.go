package server

import (
	"context"
	"strings"

	"github.com/Nahua-Foundation/ragota/pkg/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Nahua-Foundation/ragota/internal/mcp/render"
)

// formatSummary renders the service graph as the reasoning-friendly listing;
// the other two are diagram text for a human.
const formatSummary = "summary"

var serviceFormats = []string{formatSummary, client.FormatMermaid, client.FormatDOT}

// servicesLimit caps each of the two lists when the caller names no limit. The
// graph grows with every repository ever indexed, and "the map of the estate"
// stops being a map somewhere before the two-hundredth link.
const servicesLimit = 60

type servicesInput struct {
	Repos  []string `json:"repos,omitempty" jsonschema:"Restrict to these repositories, by id or by name. A link survives when either end is in the selection. Ignored by the mermaid and dot formats."`
	Limit  int      `json:"limit,omitempty" jsonschema:"Maximum services and maximum links, each counted separately. Ignored for the mermaid and dot formats."`
	Format string   `json:"format,omitempty" jsonschema:"summary (default) is the listing to reason over. mermaid and dot return diagram text for a person to look at, carry no counts or confidences, and always render the whole estate."`
}

type topicsInput struct {
	Service string `json:"service,omitempty" jsonschema:"Only topics this service is on either end of, matched exactly against the detected name. Names come from ragota_services."`
}

type statusInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"Also report contract coverage for this repository, by id or by name - how much of its outbound call surface the indexer resolved."`
}

func (s *Server) registerEstate(m *mcp.Server) {
	addTool(m, tool{
		name:  "ragota_services",
		title: "Service map",
		enums: map[string][]string{"format": serviceFormats},
		description: `
The deployables ragota detected and what they call: the map of the estate. Reach for it before asking about the code inside one service, to learn which services exist, which repositories they live in, and which of them talk to which.

A link aggregates every call site that shares a source service, a destination service, a kind and a contract key, so one link reading "http:POST /charges x3" is three call sites rather than three links. kind is http_call, rpc_call, kafka_flow, or runtime_call — the last observed in tracing data rather than in code. Links inside a single service are dropped.

Service names are unique within a repository but not across them, so two repositories may each have an "api"; every line here is therefore qualified by repository.

The unit id on each service is the way into the graph: pass it to ragota_neighbors.`,
	}, s.handleServices)

	addTool(m, tool{
		name:  "ragota_topics",
		title: "Messaging topics",
		description: `
Messaging contracts with the code on both ends: for each topic, the units that publish to it and the units that read it, with the service each belongs to. It is the asynchronous half of what ragota_services answers for calls, and the tool for "who consumes this event" — a text search would only find the string.

A topic declared in an AsyncAPI spec is listed even when nothing indexed produces or consumes it, which is how "nothing publishes this" is told apart from "we did not find the publisher".`,
	}, s.handleTopics)

	addTool(m, tool{
		name:  "ragota_status",
		title: "Index status and trust",
		description: `
What ragota has indexed and how far an empty answer can be trusted: its version, per-index document counts, and every registered repository with its id, its indexing status and when it was last indexed.

Call it when a retrieval answer came back empty or surprising, and to learn the repository ids the other tools take. A repository that is missing, still indexing or in error explains an empty answer that would otherwise read as absent code.

With repo set it also reports contract coverage for that repository: how many of its outbound call sites the indexer actually resolved into graph edges. A ratio well below 1 means an empty graph answer is the indexer's limit rather than the estate's — the difference between "there is no such call" and "we did not find it".`,
	}, s.handleStatus)
}

func (s *Server) handleServices(ctx context.Context, _ *mcp.CallToolRequest, in servicesInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	format := strings.TrimSpace(in.Format)
	if format != "" && format != formatSummary {
		diagram, err := s.c.ServicesExport(ctx, format)
		if err != nil {
			return nil, nil, s.explain("ragota_services", err)
		}
		out := diagram
		// The export route takes neither selector, so silently honouring the
		// arguments would be a lie about what was rendered.
		if len(in.Repos) > 0 || in.Limit > 0 {
			out += "\nNote: the " + format + " format always renders the whole estate; repos and limit were ignored. Use format summary to narrow.\n"
		}
		return text(out), nil, nil
	}

	// Only what the call asked for, never the server's configured default scope:
	// this tool answers "what talks to what", and the far side of a cross-service
	// call lives in another repository by definition. Narrowing the map to the
	// workspace's own repositories would hide exactly the links it exists to show.
	repos, err := s.repos.resolveAll(ctx, in.Repos)
	if err != nil {
		return nil, nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = servicesLimit
	}
	res, err := s.c.Services(ctx, &client.ServicesRequest{Repos: repos, Limit: limit})
	if err != nil {
		return nil, nil, s.explain("ragota_services", err)
	}
	return text(render.Services(res)), nil, nil
}

func (s *Server) handleTopics(ctx context.Context, _ *mcp.CallToolRequest, in topicsInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	res, err := s.c.Topics(ctx, strings.TrimSpace(in.Service))
	if err != nil {
		return nil, nil, s.explain("ragota_topics", err)
	}
	return text(render.Topics(res)), nil, nil
}

func (s *Server) handleStatus(ctx context.Context, _ *mcp.CallToolRequest, in statusInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	// This is the tool a caller reaches for *because* something looked wrong, so
	// it must not answer from a cache filled before whatever changed.
	s.repos.invalidate()
	repos, err := s.repos.list(ctx)
	if err != nil {
		return nil, nil, s.explain("ragota_status", err)
	}

	// Health and stats are reported when they answer and skipped when they do
	// not: a repository listing that arrived is worth more than a clean failure,
	// and this tool exists to be readable when the deployment is not well.
	health, _ := s.c.Health(ctx)
	stats, _ := s.c.Stats(ctx)

	var cov *client.Coverage
	if strings.TrimSpace(in.Repo) != "" {
		id, err := s.repos.resolve(ctx, in.Repo)
		if err != nil {
			return nil, nil, err
		}
		if cov, err = s.c.Coverage(ctx, id); err != nil {
			return nil, nil, s.explain("ragota_status", err)
		}
	}
	return text(render.Status(health, s.cfg.BaseURL, repos, stats, cov)), nil, nil
}
