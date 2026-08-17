package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Nahua-Foundation/ragota/pkg/client"
)

// explain turns a failure into the sentence a model should read.
//
// Every one of these reaches the model as tool-error content rather than as a
// protocol error, because the model is the only party that can act on some of
// them (a malformed argument) and the only party that must not act on others (a
// damaged index, where an empty result would otherwise be reported as absence).
// So each message says what happened *and* what follows from it.
func (s *Server) explain(op string, err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s timed out after %s. Ask for less — a lower limit, fewer hops, a smaller max_bytes — or have the operator raise RAGOTA_TIMEOUT", op, s.cfg.Timeout)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s was cancelled", op)
	}

	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		// Not something the API said: a dead connection, a TLS failure, a body
		// that was never JSON. Naming the address is what makes it fixable.
		return fmt.Errorf("%s could not reach ragota at %s: %v", op, s.cfg.BaseURL, err)
	}

	switch {
	case errors.Is(apiErr, client.ErrValidationFailed):
		// The server's own message names the offending field; the model can fix
		// its arguments and call again.
		return fmt.Errorf("%s was rejected: %s", op, apiErr.Message)
	case errors.Is(apiErr, client.ErrNotFound):
		return fmt.Errorf("%s found nothing under that identifier: %s", op, apiErr.Message)
	case errors.Is(apiErr, client.ErrUnauthorized):
		return fmt.Errorf("%s was refused: ragota did not accept the API key. An operator fixes this in RAGOTA_MCP_KEY and the server's api_keys; retrying will not help", op)
	case errors.Is(apiErr, client.ErrForbidden):
		return fmt.Errorf("%s was refused: the API key is valid but lacks the scope this route needs. An operator grants it; retrying will not help", op)
	case errors.Is(apiErr, client.ErrRateLimited):
		return fmt.Errorf("%s was rate limited%s", op, retryIn(apiErr.RetryAfter))
	case errors.Is(apiErr, client.ErrRepoBusy):
		return fmt.Errorf("%s hit a repository that is currently indexing%s", op, retryIn(apiErr.RetryAfter))
	case errors.Is(apiErr, client.ErrIndexDamaged):
		return fmt.Errorf("%s failed because ragota's search index is unreadable and has to be rebuilt by an operator (a forced reindex). No retry of this query will do better. Treat this as \"unknown\", never as \"the code is not there\"", op)
	case errors.Is(apiErr, client.ErrNotReady):
		return fmt.Errorf("%s failed: a dependency of ragota is unavailable. Retrieval answers will be missing or thin until it returns", op)
	case errors.Is(apiErr, client.ErrPayloadTooLarge):
		return fmt.Errorf("%s sent a request larger than the server accepts (%d bytes). Shorten the query", op, apiErr.LimitBytes)
	default:
		return fmt.Errorf("%s failed: %v", op, apiErr)
	}
}

func retryIn(d time.Duration) string {
	if d <= 0 {
		return ". Wait a moment before retrying"
	}
	return fmt.Sprintf(". Retry in %s", d)
}
