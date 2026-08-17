package client

import (
	"context"
	"net/url"
)

// Search answers a question phrased in prose with ranked hits.
//
// Choosing between this and Symbol is the decision that costs the most
// quality. Over 21 benchmark questions against one corpus:
//
//	Search      natural-language question   recall@1 0.524, recall@10 0.714, MRR 0.587
//	Symbol      an identifier already held  recall@1 0.667, recall@10 0.810, MRR 0.721
//
// So: "where does POST /cart/checkout go in the frontend" is a Search; the
// name `CheckoutHandler` lifted out of a stack trace or an earlier answer is a
// Symbol with SymbolRequest.Symbol set. Sending a bare identifier here, or a
// sentence there, is how a caller loses the difference.
//
// Set SearchRequest.MaxBytes or SearchRequest.Snippet when the answer is going
// into a model's context: the chunk attached to each hit is by far the largest
// part of the response, and a caller that will open the files itself does not
// need it (SnippetNone).
func (c *Client) Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error) {
	var out SearchResponse
	if err := c.post(ctx, apiPath("search"), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Context is Search plus the code graph expanded around every hit: the callers
// of what matched, and the far side of the contracts it names.
//
// Reach for it when the answer is not in one file — "what breaks if this
// endpoint changes", "who consumes this topic". When a file path and a line
// would do, Search is the cheaper call by an order of magnitude: a default
// Context call has measured over ten thousand tokens, which is why
// ContextRequest.MaxBytes and SnippetNone exist.
func (c *Client) Context(ctx context.Context, req *ContextRequest) (*ContextResponse, error) {
	var out ContextResponse
	if err := c.post(ctx, apiPath("context"), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Symbol finds symbols by identifier.
//
// Prefer SymbolRequest.Symbol when holding one identifier and not knowing
// whether it is the bare name or the qualified one: it is matched against
// either, where Name and Qualified narrow together when both are given. See
// Search for when this call is the wrong one.
func (c *Client) Symbol(ctx context.Context, req *SymbolRequest) (*SymbolResponse, error) {
	var out SymbolResponse
	if err := c.post(ctx, apiPath("nav", "symbol"), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Definition finds the definition enclosing a file position.
//
// It takes a position because that is what an editor has. A caller that holds
// a name instead wants Symbol, which does not need the file.
// DefinitionResponse.Definition is nil when nothing is defined there.
func (c *Client) Definition(ctx context.Context, req *DefinitionRequest) (*DefinitionResponse, error) {
	var out DefinitionResponse
	if err := c.post(ctx, apiPath("nav", "definition"), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// References finds the places that reference the symbol at a file position.
func (c *Client) References(ctx context.Context, req *ReferencesRequest) (*ReferencesResponse, error) {
	var out ReferencesResponse
	if err := c.post(ctx, apiPath("nav", "references"), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Neighbors returns the edges around one code unit, in both directions.
//
// The unit id comes from another call — a ContextItem's Unit, a ServiceInfo's
// UnitID, a PathStep. There is no way to name a unit by hand, and that is
// deliberate: ids are not stable across a reindex.
func (c *Client) Neighbors(ctx context.Context, req *NeighborsRequest) (*NeighborsResponse, error) {
	var out NeighborsResponse
	if err := c.post(ctx, apiPath("graph", "neighbors"), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GraphPath returns a directed path between two units, if one exists.
//
// No path is an answer rather than an error: the response is then an empty
// Steps with Length 0, not ErrNotFound.
func (c *Client) GraphPath(ctx context.Context, req *GraphPathRequest) (*GraphPathResponse, error) {
	var out GraphPathResponse
	if err := c.post(ctx, apiPath("graph", "path"), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Trace follows one parameter of one function through the calls and service
// contracts that carry it.
//
// It answers "where does this id end up", which neither retrieval nor a single
// graph hop can: the value changes name at every hop, and TraceStep.Tracked is
// what it is called there.
func (c *Client) Trace(ctx context.Context, req *TraceRequest) (*TraceResponse, error) {
	var out TraceResponse
	if err := c.post(ctx, apiPath("graph", "trace"), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ServicesRequest narrows the service graph. Both fields are query parameters
// rather than a body, and both exist to bound one call: the graph grows with
// every repository ever indexed.
type ServicesRequest struct {
	// Repos limits the graph to these repository ids. A link survives when
	// *either* end is in the selection — the far side of a cross-service call
	// lives in another repository by definition, and is usually the reason for
	// asking. Empty means every repository.
	Repos []string
	// Limit caps each of the two lists independently and sets
	// ServicesResponse.Truncated when it cuts. 0 means no cap.
	Limit int
}

// Services returns the detected services and the aggregated links between
// them. A nil request asks for everything.
func (c *Client) Services(ctx context.Context, req *ServicesRequest) (*ServicesResponse, error) {
	q := url.Values{}
	if req != nil {
		for _, id := range req.Repos {
			q.Add("repo", id)
		}
		positiveInt(q, "limit", req.Limit)
	}
	var out ServicesResponse
	if err := c.get(ctx, withQuery(apiPath("services"), q), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Diagram formats ServicesExport renders the service graph in.
const (
	// FormatMermaid is the default: a Mermaid flowchart.
	FormatMermaid = "mermaid"
	// FormatDOT is Graphviz DOT.
	FormatDOT = "dot"
)

// ServicesExport renders the service graph as diagram text. format is
// FormatMermaid (the default when empty) or FormatDOT.
//
// The response is the diagram itself, not JSON — it is meant to be shown, not
// walked. Use Services for a graph to reason over.
func (c *Client) ServicesExport(ctx context.Context, format string) (string, error) {
	q := url.Values{}
	if format != "" {
		q.Set("format", format)
	}
	return c.getText(ctx, withQuery(apiPath("services", "export"), q))
}

// Topics lists messaging topics with the code that produces and consumes each.
// A non-empty service narrows the list to the topics that service is on either
// end of.
func (c *Client) Topics(ctx context.Context, service string) (*TopicsResponse, error) {
	q := url.Values{}
	if service != "" {
		q.Set("service", service)
	}
	var out TopicsResponse
	if err := c.get(ctx, withQuery(apiPath("topics"), q), &out); err != nil {
		return nil, err
	}
	return &out, nil
}
