package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Nahua-Foundation/ragota/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Nahua-Foundation/ragota/internal/mcp/render"
)

// traceMaxDepth is the deepest this tool will ask for.
//
// The endpoint resets anything above 24 to its default of 16 rather than
// clamping, so `max_depth: 100` searches *less* deeply than `max_depth: 24` — a
// trap a model walks straight into when it wants "as deep as possible". Clamping
// here makes a larger number mean a deeper search, as it reads.
const traceMaxDepth = 24

type referencesInput struct {
	Repo     string `json:"repo" jsonschema:"The repository holding the file, by id or by name."`
	FilePath string `json:"file_path" jsonschema:"Path of the file within the repository, not an absolute path on disk."`
	Line     int    `json:"line" jsonschema:"Line in that file, 1-based. It resolves to the innermost unit containing it, so any line inside the function will do."`
	Limit    int    `json:"limit,omitempty" jsonschema:"References to return, 1 to 500. Default 50. Resolved ones take the budget first, so raising it adds only weaker ones."`
}

type neighborsInput struct {
	UnitID string `json:"unit_id" jsonschema:"Unit id from an earlier answer - ragota_context, ragota_services, ragota_path and ragota_neighbors all report them. It cannot be composed by hand and does not survive a reindex."`
}

type pathInput struct {
	FromUnitID string `json:"from_unit_id" jsonschema:"Unit id the walk starts from, out of an earlier answer."`
	ToUnitID   string `json:"to_unit_id" jsonschema:"Unit id the walk must reach, out of an earlier answer."`
	MaxDepth   int    `json:"max_depth,omitempty" jsonschema:"Hops to search, default 10."`
}

type traceInput struct {
	Symbol              string `json:"symbol" jsonschema:"The function to start from, by exact name or exact qualified name. It is not a substring search."`
	Param               string `json:"param" jsonschema:"The parameter or field to follow, as written in the source."`
	Repo                string `json:"repo,omitempty" jsonschema:"Restrict the starting symbol to one repository, by id or by name. The trace itself still crosses repositories."`
	MaxDepth            int    `json:"max_depth,omitempty" jsonschema:"Hops to follow, 1 to 24. Default 16."`
	IncludeAlternatives bool   `json:"include_alternatives,omitempty" jsonschema:"Also return the runner-up chains. Off by default; each costs as much as the best one."`
}

func (s *Server) registerGraph(m *mcp.Server) {
	addTool(m, tool{
		name:  "ragota_references",
		title: "Find references to a position",
		description: `
Where the symbol at a file position is used, across every indexed repository. Give the file and a line inside the thing you are asking about; it resolves to the innermost unit containing that line.

The answer is code-graph edges that point at that unit, so it is wider than a call list: imports, HTTP and gRPC calls, produces and consumes all appear. An edge that names the symbol but was never resolved to it is marked unresolved — a lead to check, not a fact.

Use this when you have a file and a line, which is the usual case while editing. When you have only a name, ragota_symbol finds its definition first; when you have a question, ragota_search does.`,
	}, s.handleReferences)

	addTool(m, tool{
		name:  "ragota_neighbors",
		title: "Edges around a code unit",
		description: `
The edges immediately around one code unit: what it calls, produces or writes, and what points at it, each with the unit on the far side where the edge resolved. An edge with no far side is the ordinary shape of a call into something not indexed here, such as a third-party library.

unit_id is not something you can compose. It comes back from ragota_context, ragota_services, ragota_path or an earlier ragota_neighbors, and ids do not survive a reindex, so follow one you were just given rather than one you remembered.

Use it to expand a single unit you have already located. To find the units in the first place, ragota_context expands the graph around search hits and bounds what it returns.`,
	}, s.handleNeighbors)

	addTool(m, tool{
		name:  "ragota_path",
		title: "Path between two code units",
		description: `
Whether one code unit reaches another, and through which hops. Both ids come from earlier answers — ragota_context, ragota_services or ragota_neighbors.

The walk is directed and follows resolved outgoing edges, with one exception that is the point of it: a gRPC method is also expanded to its server implementations, so a path can cross a service boundary.

Use it to confirm a connection between two units you have already identified. To follow a value rather than reachability, use ragota_trace; to find the units at all, ragota_context.

An empty answer means either that no path exists within max_depth or that from_unit_id names nothing at all — the API does not distinguish the two.`,
	}, s.handlePath)

	addTool(m, tool{
		name:  "ragota_trace",
		title: "Trace a parameter across services",
		description: `
Follow one parameter of one function through everything that carries its value: further calls, gRPC and HTTP requests, Kafka messages and database writes, across service and repository boundaries. This is the answer to "where does this order id actually end up", which neither retrieval nor a single graph hop can give, because the value is renamed at every hop and the chain is what you are asking about.

param is matched ignoring case and underscores and aligned on word boundaries, so user_id follows userId, order.UserID and req.GetUserId(), while user follows neither username nor user_agent. It is never checked against the function's real parameter list, so a misspelling comes back empty rather than as an error.

Confidence is cumulative along the chain: it falls with length, and a long chain is a lead to verify in the source rather than a fact to report. Only the best chain comes back unless include_alternatives is set.`,
	}, s.handleTrace)
}

func (s *Server) handleReferences(ctx context.Context, _ *mcp.CallToolRequest, in referencesInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.FilePath) == "" {
		return nil, nil, errors.New("file_path is empty")
	}
	if in.Line <= 0 {
		return nil, nil, errors.New("line must be 1-based and positive; line 0 always resolves to nothing")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	repo, err := s.repos.resolve(ctx, in.Repo)
	if err != nil {
		return nil, nil, err
	}
	if repo == "" {
		return nil, nil, errors.New("repo is required: a file path alone does not identify a file across repositories")
	}

	res, err := s.c.References(ctx, &client.ReferencesRequest{
		RepoID:   repo,
		FilePath: in.FilePath,
		Position: client.Position{Line: in.Line},
		Limit:    in.Limit,
	})
	if err != nil {
		return nil, nil, s.explain("ragota_references", err)
	}
	return text(render.References(res)), nil, nil
}

func (s *Server) handleNeighbors(ctx context.Context, _ *mcp.CallToolRequest, in neighborsInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.UnitID) == "" {
		return nil, nil, errors.New("unit_id is empty: take one from a ragota_context, ragota_services or ragota_neighbors answer")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	res, err := s.c.Neighbors(ctx, &client.NeighborsRequest{UnitID: strings.TrimSpace(in.UnitID)})
	if err != nil {
		// An id the server does not know is almost always one carried over from
		// before a reindex rather than a typo, and a model told only "not found"
		// will retry it. Say which of the two it is likely to be.
		if errors.Is(err, client.ErrNotFound) {
			return nil, nil, fmt.Errorf("no unit %q. Unit ids do not survive a reindex, so fetch a fresh one from ragota_context, ragota_symbol or ragota_services rather than reusing this",
				strings.TrimSpace(in.UnitID))
		}
		return nil, nil, s.explain("ragota_neighbors", err)
	}
	return text(render.Neighbors(res)), nil, nil
}

func (s *Server) handlePath(ctx context.Context, _ *mcp.CallToolRequest, in pathInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.FromUnitID) == "" || strings.TrimSpace(in.ToUnitID) == "" {
		return nil, nil, errors.New("from_unit_id and to_unit_id are both required, and both come from an earlier answer")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	res, err := s.c.GraphPath(ctx, &client.GraphPathRequest{
		FromUnitID: strings.TrimSpace(in.FromUnitID),
		ToUnitID:   strings.TrimSpace(in.ToUnitID),
		MaxDepth:   in.MaxDepth,
	})
	if err != nil {
		return nil, nil, s.explain("ragota_path", err)
	}
	return text(render.Path(res)), nil, nil
}

func (s *Server) handleTrace(ctx context.Context, _ *mcp.CallToolRequest, in traceInput) (*mcp.CallToolResult, any, error) {
	symbol, param := strings.TrimSpace(in.Symbol), strings.TrimSpace(in.Param)
	if symbol == "" || param == "" {
		return nil, nil, errors.New("symbol and param are both required: ragota_trace follows one named value out of one named function")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	repo, err := s.repos.resolve(ctx, in.Repo)
	if err != nil {
		return nil, nil, err
	}

	depth := in.MaxDepth
	if depth > 0 {
		depth = clamp(depth, 1, traceMaxDepth)
	}
	res, err := s.c.Trace(ctx, &client.TraceRequest{
		RepoID:   repo,
		Symbol:   symbol,
		Param:    param,
		MaxDepth: depth,
	})
	if err != nil {
		return nil, nil, s.explain("ragota_trace", err)
	}
	return text(render.Trace(res, symbol, in.IncludeAlternatives)), nil, nil
}
