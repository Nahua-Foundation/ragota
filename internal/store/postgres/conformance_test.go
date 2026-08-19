package postgres

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/store"
	"github.com/Nahua-Foundation/ragota/internal/store/conform"
)

// TestConformance runs the shared storage suite against Postgres. Without
// RAGOTA_TEST_POSTGRES_DSN each subtest skips (see openTestStore); with it set —
// `make test-postgres` spins up a throwaway instance — this is what keeps the
// primary backend answering exactly like SQLite.
func TestConformance(t *testing.T) {
	conform.Run(t, func(t *testing.T) store.Storage {
		return openTestStore(t)
	})
}
