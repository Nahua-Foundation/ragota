package client

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Machine-readable error codes. Every non-2xx JSON body carries one, and it is
// what a client branches on: the human-readable message beside it is free to
// change between releases.
//
// internal/server/api serves these same constants, and openapi_test.go checks the set
// against the `code` enum of the served spec in both directions, so a code that
// exists here but nowhere else cannot survive a build.
const (
	CodeRepoBusy         = "repo_busy"
	CodeCommitGap        = "commit_gap"
	CodePayloadTooLarge  = "payload_too_large"
	CodeInvalidPath      = "invalid_path"
	CodeNotFound         = "not_found"
	CodeValidationFailed = "validation_failed"
	CodeRateLimited      = "rate_limited"
	CodeUnauthorized     = "unauthorized"
	CodeForbidden        = "forbidden"
	CodeInternal         = "internal_error"
	CodeNotReady         = "not_ready"
	CodeIndexDamaged     = "index_damaged"
)

// ErrorResponse is the body of every non-2xx JSON response. Callers handle
// *Error instead; this is the shape it is decoded from.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	// LastCommit is the repo's current commit cursor, set on commit_gap so the
	// client knows which range to resend.
	LastCommit string `json:"last_commit,omitempty"`
	// LimitBytes is the accepted request body size, set on payload_too_large.
	LimitBytes int64 `json:"limit_bytes,omitempty"`
	// RetryAfterSeconds is set on repo_busy and rate_limited.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// Error is what every call returns for a non-2xx answer.
//
// Match it by code rather than by message:
//
//	if errors.Is(err, client.ErrRepoBusy) { ... }
//
// and reach into the struct when the details matter:
//
//	var apiErr *client.Error
//	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusRequestEntityTooLarge {
//		log.Printf("server accepts %d bytes", apiErr.LimitBytes)
//	}
type Error struct {
	// StatusCode is the HTTP progress. It is kept alongside Code because the two
	// are not redundant: 409 is repo_busy or commit_gap, and a proxy in front
	// of the server can answer with a status and no code at all.
	StatusCode int
	// Code is one of the Code* constants, or empty when the response carried
	// no JSON body this package could read.
	Code string
	// Message is the server's human-readable text. Do not branch on it.
	Message string
	// LastCommit is the repository's current cursor, set on CodeCommitGap: the
	// range to resend starts here.
	LastCommit string
	// LimitBytes is the request body size the endpoint accepts, set on
	// CodePayloadTooLarge.
	LimitBytes int64
	// RetryAfter is how long the server asked the caller to wait, set on
	// CodeRateLimited and CodeRepoBusy. Zero means it said nothing.
	RetryAfter time.Duration
	// Body is the raw response body, for the case where it was not the JSON
	// error shape at all — an HTML error page from a proxy, say. It is what
	// makes such a failure diagnosable instead of "internal error".
	Body string
}

func (e *Error) Error() string {
	if e.StatusCode == 0 && e.Message == "" && e.Body == "" {
		// A bare sentinel: it names a condition, not an occurrence.
		return e.Code
	}
	// An answer that was not the JSON error shape has no message, and the
	// status alone does not say what went wrong. Quoting the start of the body
	// is what makes a proxy's error page diagnosable.
	detail := e.Message
	if detail == "" {
		detail = excerpt(e.Body)
	}
	switch {
	case detail == "" && e.Code == "":
		return fmt.Sprintf("ragota: http %d", e.StatusCode)
	case detail == "":
		return fmt.Sprintf("ragota: http %d (%s)", e.StatusCode, e.Code)
	case e.Code == "":
		return fmt.Sprintf("ragota: http %d: %s", e.StatusCode, detail)
	default:
		return fmt.Sprintf("ragota: http %d (%s): %s", e.StatusCode, e.Code, detail)
	}
}

// excerptLen bounds the part of an unstructured error body that reaches the
// error message. Enough to recognise an HTML error page or a proxy's text;
// short enough that logging it does not bury the rest of the line.
const excerptLen = 160

// excerpt renders body as a single short line.
func excerpt(body string) string {
	s := strings.Join(strings.Fields(body), " ")
	if len(s) > excerptLen {
		return s[:excerptLen] + "..."
	}
	return s
}

// Is reports whether target names the same failure. Two errors match when they
// carry the same code, which is what makes the sentinels below usable with
// errors.Is: the sentinel carries a code and nothing else.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// The conditions a caller can act on, as values. `errors.Is(err, ErrRepoBusy)`
// beats matching on a message that is documented to change.
//
// They are sentinels, not occurrences: they carry the code and no status,
// message or retry hint. Use errors.As to reach those on the error you were
// handed.
var (
	// ErrRepoBusy — an index pass is already running for this repository. A
	// retry condition; the returned error carries RetryAfter.
	ErrRepoBusy = &Error{Code: CodeRepoBusy}
	// ErrCommitGap — the pushed batch does not continue the stored cursor.
	// Error.LastCommit is where the missing range starts.
	ErrCommitGap = &Error{Code: CodeCommitGap}
	// ErrPayloadTooLarge — the request body exceeds the endpoint's limit,
	// which Error.LimitBytes reports.
	ErrPayloadTooLarge = &Error{Code: CodePayloadTooLarge}
	// ErrInvalidPath — a path in the request escapes the repository.
	ErrInvalidPath = &Error{Code: CodeInvalidPath}
	// ErrNotFound — no such repository, job or unit.
	ErrNotFound = &Error{Code: CodeNotFound}
	// ErrValidationFailed — the request is malformed or missing a required
	// field. Retrying it unchanged cannot help.
	ErrValidationFailed = &Error{Code: CodeValidationFailed}
	// ErrRateLimited — the caller's bucket is empty. Error.RetryAfter says for
	// how long; the client already waits and retries once per configured
	// attempt before surfacing this.
	ErrRateLimited = &Error{Code: CodeRateLimited}
	// ErrUnauthorized — no API key, or one the server does not know.
	ErrUnauthorized = &Error{Code: CodeUnauthorized}
	// ErrForbidden — the key is valid but lacks the scope the route requires.
	// The scope prefix lives in the server's configuration, so this is fixed
	// by the operator granting the key "admin:", never by the client.
	ErrForbidden = &Error{Code: CodeForbidden}
	// ErrInternal — the server failed for a reason it does not expose.
	ErrInternal = &Error{Code: CodeInternal}
	// ErrNotReady — a dependency of the server is unavailable. Liveness is
	// unaffected: Health still answers.
	ErrNotReady = &Error{Code: CodeNotReady}
	// ErrIndexDamaged — the search index is unreadable and must be rebuilt
	// with a forced reindex. No retry of the same query will do better, and a
	// caller measuring retrieval quality must not read the empty result as a
	// ranking regression.
	ErrIndexDamaged = &Error{Code: CodeIndexDamaged}
)

// codeForStatus is the code to assume when a response carried no readable JSON
// body — a proxy's 502 page, a truncated body. The status is all there is to go
// on, and answering with an empty code would push every such failure into the
// caller's default branch.
func codeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusRequestEntityTooLarge:
		return CodePayloadTooLarge
	case http.StatusTooManyRequests:
		return CodeRateLimited
	case http.StatusBadRequest:
		return CodeValidationFailed
	default:
		if status >= 500 {
			return CodeInternal
		}
		return ""
	}
}
