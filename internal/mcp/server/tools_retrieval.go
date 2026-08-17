package server

import (
	"context"
	"errors"
	"strings"

	"github.com/Nahua-Foundation/ragota/pkg/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Nahua-Foundation/ragota/internal/mcp/render"
)

// The value sets ragota validates strictly. An unrecognised one is a 400,
// never a quiet downgrade, so the model is shown the set rather than left to
// discover it.
var (
	snippetModes = []string{client.SnippetChunk, client.SnippetLine, client.SnippetNone}
	intents      = []string{"auto", "callers", "none"}
)

// Argument documentation is repeated verbatim in the struct tags below rather
// than shared through a constant: a struct tag must be a literal, and the
// alternative — building each schema by hand — is what lets a declared argument
// drift away from the field that reads it.
//
// It is also kept short. Descriptions and schemas of every registered tool sit in
// the model's context on every turn, so an argument gets the sentence that
// changes a decision and nothing more; the reasoning behind a default belongs in
// the tool description, which is read once for the whole tool rather than once
// per field.
type searchInput struct {
	Query      string   `json:"query" jsonschema:"The question, phrased as a sentence, as close to how it was put to you as possible. Not a keyword list, and not a bare identifier - send an identifier to ragota_symbol."`
	Repos      []string `json:"repos,omitempty" jsonschema:"Restrict to these repositories, by id or by name; ids come from ragota_status or from any hit. Omitted means all of them."`
	Limit      int      `json:"limit,omitempty" jsonschema:"Hits to retrieve, 1 to 100. Default 10."`
	Snippet    string   `json:"snippet,omitempty" jsonschema:"Code per hit: line (default) is the anchor line, chunk is the whole indexed chunk and several times larger, none gives locations only."`
	MaxBytes   int      `json:"max_bytes,omitempty" jsonschema:"Cap on the answer in bytes. Weakest hits are dropped until it fits, and the tool says when that happened."`
	Intent     string   `json:"intent,omitempty" jsonschema:"auto (default) reads the phrasing; callers resolves the symbol the query describes through the code graph and answers with its call sites, which plain retrieval cannot; none forces plain retrieval. Say callers explicitly when you mean it."`
	Languages  []string `json:"languages,omitempty" jsonschema:"Keep only hits in these languages, for example go or java. A hit of unknown language never satisfies this."`
	Kinds      []string `json:"kinds,omitempty" jsonschema:"Keep only hits of these unit kinds, for example function, method, class, http_route, rpc_method, db_table."`
	PathPrefix string   `json:"path_prefix,omitempty" jsonschema:"Keep only hits whose file path starts with this, case-sensitively, for example services/orders."`
}

type contextInput struct {
	Query    string   `json:"query" jsonschema:"The question, phrased as a sentence. Ask what you want to know about the code, not a keyword list."`
	Repos    []string `json:"repos,omitempty" jsonschema:"Restrict to these repositories, by id or by name; ids come from ragota_status or from any hit. Omitted means all of them."`
	Limit    int      `json:"limit,omitempty" jsonschema:"Hits to expand, 1 to 20. Default 5. Each one costs a whole graph expansion."`
	Hops     int      `json:"hops,omitempty" jsonschema:"Expansion depth around each hit, 1 to 3. Default 1, and cost grows quickly with it."`
	Snippet  string   `json:"snippet,omitempty" jsonschema:"Code per hit: line (default) is the anchor line, chunk is the whole indexed chunk and several times larger, none gives locations only."`
	MaxBytes int      `json:"max_bytes,omitempty" jsonschema:"Cap on the answer in bytes. Whole items are dropped, weakest first, so a surviving hit keeps the expansion that explains it."`
	Intent   string   `json:"intent,omitempty" jsonschema:"auto (default) reads the phrasing; callers resolves the symbol the query describes through the code graph and answers with its call sites, which plain retrieval cannot; none forces plain retrieval. Say callers explicitly when you mean it."`
}

type symbolInput struct {
	Symbol string `json:"symbol,omitempty" jsonschema:"The identifier, exactly as you hold it. It is matched against the bare name and against the fully qualified name, so do not try to work out which one you have."`
	Repo   string `json:"repo,omitempty" jsonschema:"Restrict to one repository, by id or by name."`
	Kind   string `json:"kind,omitempty" jsonschema:"Restrict to one unit kind, for example function, method, class, rpc_method, http_route, db_table, config_key. With symbol omitted this enumerates that kind instead."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Symbols to return, 1 to 500. Default 50."`
}

func (s *Server) registerRetrieval(m *mcp.Server) {
	addTool(m, tool{
		name:  "ragota_search",
		title: "Search code by question",
		enums: map[string][]string{"snippet": snippetModes, "intent": intents},
		description: `
Ranked code locations for a question asked in prose, across every indexed repository: "where does POST /cart/checkout go in the frontend", "how is the retry budget configured", "what validates a discount code".

Choose between this and ragota_symbol by what you are holding, not by what you want to end up with:
  - a QUESTION, phrased as a sentence -> ragota_search
  - an IDENTIFIER you already have, out of a stack trace, a diff, a log line or an earlier answer -> ragota_symbol
Measured over the same 21 benchmark questions on one corpus, this endpoint answers a prose question at MRR 0.587 while ragota_symbol answers a known identifier at 0.714, and the ordering reverses when the input is the other kind. Choosing wrong is the largest avoidable loss of answer quality on this server. This endpoint also applies no exact-over-substring tiering and does not demote generated files, so an identifier sent here meets the generated stubs that share its name.

This is not a grep. It resolves what the question is about — an HTTP route, a gRPC method, a Kafka topic, a database table — and promotes the code that carries that contract, so ask the question as it was put to you rather than decomposing it into keywords first. Use your own file search for a literal string you already know is in the source.

The answer is budgeted: snippet defaults to one line of code per hit and max_bytes to this server's default. Raise them only when you will not open the files yourself.

An empty answer is always explained. The tool reports whether retrieval ran whole or degraded, and a zero-hit answer under degradation is not evidence that the code is absent — do not report it as one.`,
	}, s.handleSearch)

	addTool(m, tool{
		name:  "ragota_symbol",
		title: "Look up a symbol by name",
		description: `
Find symbols by identifier: a name you already hold, out of a stack trace, a diff, a log line, an error message or an earlier answer. The identifier is matched against the bare name and against the fully qualified name, so pass it exactly as you have it rather than guessing which one it is.

Use this INSTEAD of ragota_search whenever your input is a name rather than a question. Measured over the same corpus, it finds a known identifier at recall@1 0.667 and MRR 0.714, against ragota_search's 0.524 and 0.587; a question phrased as a sentence belongs in ragota_search, where the numbers reverse.

Matching is on the name, case-insensitively, and never on meaning — it will not find "the function that retries failed payments". Exact matches are returned first and in full, and generated code and test files rank below hand-written code, so a query for a known name finds the implementation before the generated stub that shares it. ragota_search does neither of those.

With symbol omitted this enumerates a kind instead: kind db_table with a repo lists that repository's tables, kind http_route its declared routes.`,
	}, s.handleSymbol)

	addTool(m, tool{
		name:  "ragota_context",
		title: "Search with the code graph expanded",
		enums: map[string][]string{"snippet": snippetModes, "intent": intents},
		description: `
ragota_search plus the code graph expanded around every hit: for each result, the unit it lands in, the service that unit belongs to, and the units the graph reaches from it — its callers, the handler behind a route, the consumers of a topic it publishes, the far side of a gRPC contract.

Reach for it when the answer is not in one file: "what breaks if this endpoint changes", "who consumes this event", "what does this handler end up calling". When a file path and a line would do, ragota_search costs a small fraction as much: this is by far the most expensive call on this server, and against a corpus of three small repositories a default call has measured over ten thousand tokens.

hops is the expansion depth and multiplies that cost. Leave it at 1 unless one hop demonstrably did not reach the answer.

Items are dropped whole when the byte budget binds, so every surviving hit keeps the expansion that explains it.`,
	}, s.handleContext)
}

func (s *Server) handleSearch(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, nil, errors.New("query is empty: ragota_search needs a question phrased as a sentence")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	repos, err := s.scope(ctx, in.Repos)
	if err != nil {
		return nil, nil, err
	}

	res, err := s.c.Search(ctx, &client.SearchRequest{
		Query:    in.Query,
		Repos:    repos,
		Limit:    in.Limit,
		Filter:   searchFilter(in.Languages, in.Kinds, in.PathPrefix),
		Intent:   in.Intent,
		Snippet:  snippetMode(in.Snippet),
		MaxBytes: s.budget(in.MaxBytes),
		// Asked for on every call, not only when something looks wrong, because
		// the caller cannot tell that it looks wrong: a degraded answer and a
		// thin one are the same hit list. A few hundred bytes against the byte
		// budget buys the difference between "not in the corpus" and "one index
		// was down", and the renderer prints it only when it changes the answer.
		Diagnostics: true,
	})
	if err != nil {
		return nil, nil, s.explain("ragota_search", err)
	}
	return text(render.Search(res)), nil, nil
}

func (s *Server) handleContext(ctx context.Context, _ *mcp.CallToolRequest, in contextInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, nil, errors.New("query is empty: ragota_context needs a question phrased as a sentence")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	repos, err := s.scope(ctx, in.Repos)
	if err != nil {
		return nil, nil, err
	}

	res, err := s.c.Context(ctx, &client.ContextRequest{
		Query:    in.Query,
		Repos:    repos,
		Limit:    in.Limit,
		Hops:     in.Hops,
		Intent:   in.Intent,
		Snippet:  snippetMode(in.Snippet),
		MaxBytes: s.budget(in.MaxBytes),
	})
	if err != nil {
		return nil, nil, s.explain("ragota_context", err)
	}

	out := render.Context(res)
	if len(res.Items) == 0 {
		out += "\n" + s.probeRetrieval(ctx, in.Query, repos)
	}
	return text(out), nil, nil
}

// probeRetrieval asks /search whether retrieval is whole.
//
// /context carries no diagnostics of its own, so an empty package there is
// ambiguous in exactly the way /search's `degraded` exists to resolve — and this
// is the tool whose empty answer an agent is most likely to report as "nothing
// calls it". One extra cheap request buys that distinction, and only on the path
// where the distinction changes what the caller should conclude.
func (s *Server) probeRetrieval(ctx context.Context, query string, repos []string) string {
	res, err := s.c.Search(ctx, &client.SearchRequest{
		Query:       query,
		Repos:       repos,
		Limit:       1,
		Snippet:     client.SnippetNone,
		MaxBytes:    2 << 10,
		Diagnostics: true,
	})
	if err != nil || res.Diagnostics == nil {
		return "Retrieval health could not be checked, so an empty answer here cannot be told apart from a backend that was down."
	}
	return render.Diagnostics(res.Diagnostics, true)
}

func (s *Server) handleSymbol(ctx context.Context, _ *mcp.CallToolRequest, in symbolInput) (*mcp.CallToolResult, any, error) {
	// The server rejects a request carrying no selector at all, since an
	// unfiltered one would page through the whole symbol table. Saying so here
	// costs no round trip and names the fields this tool actually has.
	if strings.TrimSpace(in.Symbol) == "" && strings.TrimSpace(in.Kind) == "" && strings.TrimSpace(in.Repo) == "" {
		return nil, nil, errors.New("give at least one of symbol, kind or repo: an unfiltered lookup would page through every symbol in the corpus")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	repo, err := s.repos.resolve(ctx, in.Repo)
	if err != nil {
		return nil, nil, err
	}

	res, err := s.c.Symbol(ctx, &client.SymbolRequest{
		RepoID: repo,
		Symbol: strings.TrimSpace(in.Symbol),
		Kind:   strings.TrimSpace(in.Kind),
		Limit:  in.Limit,
	})
	if err != nil {
		return nil, nil, s.explain("ragota_symbol", err)
	}
	return text(render.Symbols(res, in.Symbol)), nil, nil
}

// searchFilter builds the endpoint's filter object out of the flat arguments
// this tool exposes.
//
// The wire shape is a free-form map whose unknown keys are ignored rather than
// rejected, which is the one shape a model must not be handed: a misspelled key
// silently stops filtering. Flat, named arguments make the legal set part of the
// tool schema instead.
func searchFilter(languages, kinds []string, pathPrefix string) map[string]any {
	filter := map[string]any{}
	if len(languages) > 0 {
		filter["languages"] = languages
	}
	if len(kinds) > 0 {
		filter["kinds"] = kinds
	}
	if p := strings.TrimSpace(pathPrefix); p != "" {
		filter["path_prefix"] = p
	}
	if len(filter) == 0 {
		return nil
	}
	return filter
}
