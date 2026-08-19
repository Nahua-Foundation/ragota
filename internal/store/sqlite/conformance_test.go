package sqlite

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/store"
	"github.com/Nahua-Foundation/ragota/internal/store/conform"
)

// TestConformance runs the shared storage suite against SQLite. The same suite
// runs against Postgres, so behaviour the service layer relies on cannot drift
// between the two backends unnoticed.
func TestConformance(t *testing.T) {
	conform.Run(t, func(t *testing.T) store.Storage {
		return openTestDB(t)
	})
}
