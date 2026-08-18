// Package server exposes a running ragota to a coding agent as MCP tools.
//
// # The tool set
//
// Ten tools, all read-only. The set was chosen by asking what a coding agent
// cannot already do for itself — it has a filesystem, a grep and a language
// server — and every tool here answers a question that needs the cross-repository
// code graph or the ranking ragota builds over it.
//
// Two of them are deliberately not merged, because measurement says the split is
// worth keeping: over 21 benchmark questions on one corpus, /search answers a
// prose question at MRR 0.587 and /nav/symbol answers an identifier the caller
// already holds at 0.714, and the order reverses when the input is the other
// kind. That is a quality difference a caller can lose by picking wrong, so
// ragota_search and ragota_symbol each say, in the description the model reads,
// which input belongs where.
//
// # Nothing here mutates
//
// ragota has read and admin key scopes precisely so that a client acting
// for a language model cannot be talked into a DELETE, and this server takes
// that at its word: no tool reaches a route that changes anything, and there is
// no configuration flag that adds one.
//
// The capability that would tempt one is "reindex after my change", and the
// deployment already answers it better — a git webhook, or the commit-push API a
// dev tool drives. Adding it here would make an operator hand a model-facing
// process an admin key, which also permits deleting a repository and everything
// indexed from it. That is a poor trade for a convenience already covered.
package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/Nahua-Foundation/ragota/client"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Nahua-Foundation/ragota/internal/mcp/config"
)

// Server holds what every tool handler needs.
type Server struct {
	cfg   *config.Config
	c     *client.Client
	repos *repoIndex
}

// New builds a Server over an already-constructed client.
func New(cfg *config.Config, c *client.Client) *Server {
	return &Server{cfg: cfg, c: c, repos: newRepoIndex(c)}
}

// ToolNames lists every tool this server registers, in registration order. It
// exists so that a test can assert the set without a live session, and so that
// the README table has one source.
func ToolNames() []string {
	return []string{
		"ragota_search",
		"ragota_symbol",
		"ragota_context",
		"ragota_references",
		"ragota_neighbors",
		"ragota_path",
		"ragota_trace",
		"ragota_services",
		"ragota_topics",
		"ragota_status",
	}
}

// MCP builds the MCP server with every tool registered.
func (s *Server) MCP(version string) *mcp.Server {
	m := mcp.NewServer(&mcp.Implementation{
		Name:    "ragota",
		Title:   "ragota code graph",
		Version: version,
		Description: "Cross-repository code retrieval over a running ragota: ranked search, symbol lookup, " +
			"the call/HTTP/gRPC/Kafka/table graph around a unit, parameter tracing across service boundaries, " +
			"and the service map.",
	}, nil)

	s.registerRetrieval(m)
	s.registerGraph(m)
	s.registerEstate(m)
	return m
}

// StartupCheck proves the whole path to ragota before the first tool call:
// that it answers, that it speaks a contract this build understands, that the
// key is accepted, and that any configured repository scope names real
// repositories.
//
// It is done at startup rather than lazily because an MCP client shows a startup
// failure to the user and hides a tool failure inside a model's turn — and
// because a version mismatch discovered through a decode error reads as a bug in
// this program.
func (s *Server) StartupCheck(ctx context.Context) (*client.HealthResponse, error) {
	// /health carries no credential, so a failure here is the network or the
	// address and never the key. That separation is the whole reason to make two
	// calls instead of one.
	health, err := s.c.CheckCompatibility(ctx)
	if err != nil && health == nil {
		return nil, fmt.Errorf("cannot reach ragota at %s: %w\nCheck that it is running and that RAGOTA_URL points at it", s.cfg.BaseURL, err)
	}
	if err != nil {
		return health, fmt.Errorf("ragota at %s speaks an API this build cannot use: %w\nUpgrade whichever of the two is older", s.cfg.BaseURL, err)
	}

	// A read-scoped route, so a wrong or missing key fails here rather than
	// inside the model's first question.
	repos, err := s.repos.list(ctx)
	if err != nil {
		return health, s.explain("the startup check", err)
	}

	if _, err := s.repos.resolveAll(ctx, s.cfg.Repos); err != nil {
		return health, fmt.Errorf("RAGOTA_REPOS: %w", err)
	}
	if len(repos) == 0 {
		// Not fatal: an operator may be about to add one, and refusing to start
		// would make this server the thing that looks broken.
		return health, nil
	}
	return health, nil
}

// --- registration helpers ---

// readOnly marks every tool here as one that changes nothing, which is true of
// all of them and lets a client skip a confirmation it does not need.
//
// IdempotentHint is left unset rather than set: the spec defines it only for a
// tool that is *not* read-only, so on these it would be a field that costs bytes
// in every tool listing and says nothing.
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true}
}

// tool describes one tool, with the enum constraints that the inferred schema
// cannot carry.
//
// The enums matter more than they look: ragota rejects an unrecognised
// mode, snippet or intent with 400 rather than quietly downgrading it, so a
// value the model invented costs a round trip and a confusing error. Declaring
// the legal set is how the model is told instead of discovering.
type tool struct {
	name        string
	title       string
	description string
	enums       map[string][]string
}

func addTool[In any](m *mcp.Server, t tool, h func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error)) {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("tool %s: input schema: %v", t.name, err))
	}
	for field, values := range t.enums {
		prop, ok := schema.Properties[field]
		if !ok {
			panic(fmt.Sprintf("tool %s: enum for unknown field %q", t.name, field))
		}
		prop.Enum = make([]any, 0, len(values))
		for _, v := range values {
			prop.Enum = append(prop.Enum, v)
		}
	}
	mcp.AddTool(m, &mcp.Tool{
		Name:        t.name,
		Description: strings.TrimSpace(t.description),
		Annotations: readOnly(t.title),
		InputSchema: schema,
	}, h)
}

// text is the only result shape these tools produce. No output schema is
// declared: a structured result would be echoed as JSON in the content block
// too, and paying twice for the same answer is the opposite of what the byte
// budgets here exist for.
func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// --- shared argument handling ---

// scope resolves a call's repository selection, falling back to the configured
// default. Names are accepted alongside ids because a model has no way to invent
// an id, and a wrong id is answered by ragota with zero hits rather than an
// error — a scoping typo that reads as absent code.
func (s *Server) scope(ctx context.Context, repos []string) ([]string, error) {
	if len(repos) == 0 {
		repos = s.cfg.Repos
	}
	return s.repos.resolveAll(ctx, repos)
}

// budget applies the configured default when the caller named no cap. Zero is
// not passed through: to ragota it means "no cap at all", and an uncapped
// /context has measured over ten thousand tokens.
func (s *Server) budget(maxBytes int) int {
	if maxBytes > 0 {
		return maxBytes
	}
	return s.cfg.MaxBytes
}

// snippetMode defaults the code body to one line per hit.
//
// The server's own default is the whole indexed chunk, which is the largest
// thing in a response — one measured at 2,420 bytes against ~120 for the hit
// around it. A coding agent can open the file itself; what it cannot do cheaply
// is decide which of ten files to open, and one line is enough for that.
func snippetMode(mode string) string {
	if mode == "" {
		return client.SnippetLine
	}
	return mode
}

// clamp keeps a numeric argument inside the range the endpoint honours.
func clamp(v, lo, hi int) int {
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	default:
		return v
	}
}
