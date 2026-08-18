// Package client is the Go client for the ragota HTTP API.
//
// It exists so that a program outside this module — an MCP server, a CLI, an
// evaluation harness — does not have to hand-roll requests and re-declare the
// response structs. The wire types in this package are the ones the server
// serves: internal/api aliases them rather than keeping its own copies, so
// there is one definition of each and no way for the two sides to drift.
//
// # Getting started
//
//	c := client.New("http://localhost:8080", client.WithAPIKey(key))
//	if _, err := c.CheckCompatibility(ctx); err != nil {
//		return err
//	}
//	res, err := c.Search(ctx, &client.SearchRequest{
//		Query:    "where does POST /cart/checkout go in the frontend",
//		Limit:    10,
//		Snippet:  client.SnippetNone,
//		MaxBytes: 16 << 10,
//	})
//
// # Which call to make
//
// The two retrieval calls have opposite strengths, and confusing them is the
// single largest quality loss available to a caller:
//
//   - [Client.Search] for a question phrased in prose.
//   - [Client.Symbol] for an identifier the caller already holds — a name out
//     of a stack trace, a diff, or an earlier answer — passed in
//     SymbolRequest.Symbol, which is matched against the bare name or the
//     qualified one.
//
// [Client.Context] is Search plus the code graph expanded around every hit.
// Reach for it when the answer needs the callers of what matched or the far
// side of a contract, and not when a file path would have done: it is by far
// the most expensive response the API produces.
//
// # Errors
//
// Every non-2xx answer becomes an [Error] carrying the server's machine-readable
// code and the HTTP status. Branch on the code with errors.Is:
//
//	res, err := c.Search(ctx, req)
//	switch {
//	case errors.Is(err, client.ErrRateLimited):
//		// Error.RetryAfter says how long; the client has already waited and
//		// retried as configured before surfacing this.
//	case errors.Is(err, client.ErrIndexDamaged):
//		// A forced reindex is the repair. No retry of this query will do better,
//		// and the empty result is not a ranking regression.
//	case errors.Is(err, client.ErrForbidden):
//		// The key is valid but lacks the scope; the operator grants it, not us.
//	}
//
// Use errors.As for the details — [Error.RetryAfter], [Error.LimitBytes],
// [Error.LastCommit]. Failures that are not answers (a dead connection, a
// cancelled context) are returned unchanged, because the API did not say them.
//
// # A degraded search is not an error
//
// A search fails only when every index fails it — [ErrIndexDamaged] when one of
// them is unreadable, [ErrInternal] otherwise. When just one of them is down the
// query is still answered, from fewer indexes than the deployment has, and the
// result is indistinguishable from an ordinary thin one. Set
// [SearchRequest].Diagnostics to be told: [SearchDiagnostics].Degraded is what
// separates a corpus with no answer from a retrieval backend that had none left
// to give. It is off by default because it costs bytes on every response that
// carries it, and most callers will not act on it.
//
// # Retries and rate limiting
//
// Repeatable calls — every read, which is most of this API even where the verb
// is POST — are retried on connection errors, 429 and 5xx, with an exponential
// backoff that honours a Retry-After the server sends. See [WithRetries] and
// [WithBackoff].
//
// [Client.AddRepo] and [Client.Index] change server state and are sent exactly
// once whatever the retry configuration says, because a repeated Index reports
// the state the first attempt created rather than what it did.
//
// # Authentication and scopes
//
// The API key goes in with [WithAPIKey] (or [WithBearerToken] behind a gateway
// that only forwards Authorization). Send the key itself: the "read:" and
// "admin:" prefixes in the server's configuration record what a key may do and
// are not part of the credential.
//
// A read-scoped key reaches every retrieval and inspection route and nothing
// that mutates — which is the point, since a retrieval client acts for a
// language model. [Client.AddRepo] and [Client.Index] need an admin key and
// answer [ErrForbidden] without one.
package client
