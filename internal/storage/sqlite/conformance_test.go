package sqlite

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/storage/storagetest"
)

// TestConformance runs the shared storage suite against SQLite. The same suite
// runs against Postgres, so behaviour the service layer relies on cannot drift
// between the two backends unnoticed.
func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage {
		return openTestDB(t)
	})
}
