package testutil

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Nahua-Foundation/ragota/internal/api"
	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/service"
	"github.com/Nahua-Foundation/ragota/internal/setup"
)

// schemaSeq disambiguates test schemas created within the same nanosecond.
var schemaSeq atomic.Int64

// WebhookSecret is the shared secret SetupServer configures for /webhooks/git.
const WebhookSecret = "e2e-webhook-secret"

// TestConfig returns a config for tests. By default it uses in-memory SQLite;
// set RAGOTA_TEST_STORAGE=postgres and RAGOTA_TEST_POSTGRES_DSN (URL form,
// e.g. postgres://postgres:postgres@localhost:5432/ragota_test?sslmode=disable)
// to run the same tests against PostgreSQL. In postgres mode each call gets a
// fresh, isolated schema via search_path, dropped on test cleanup.
func TestConfig(t *testing.T) *config.Config {
	t.Helper()

	storageCfg := config.StorageConfig{
		SQLite: &config.SQLiteStorageConfig{
			Path:     ":memory:",
			PoolSize: 1,
		},
	}
	if os.Getenv("RAGOTA_TEST_STORAGE") == "postgres" {
		if dsn := os.Getenv("RAGOTA_TEST_POSTGRES_DSN"); dsn != "" {
			storageCfg = config.StorageConfig{
				Postgres: &config.PostgresStorageConfig{
					DSN:      postgresTestDSN(t, dsn),
					PoolSize: 4,
				},
			}
		}
	}

	return &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 0,
		},
		Storage: storageCfg,
		Indexes: config.IndexesConfig{
			AST: &config.ASTIndexConfig{
				// Left empty on purpose: the list restricts which parsers are
				// registered, and the e2e fixtures are polyglot.
				Enabled: true,
			},
			BM25: &config.BM25IndexConfig{
				Enabled: true,
			},
		},
		Models: config.ModelsConfig{
			Providers: map[string]config.ProviderConfig{},
		},
		Repos: config.ReposConfig{
			Sources: config.ReposSourcesConfig{
				Local: &config.LocalSourceConfig{
					Enabled: true,
				},
			},
		},
	}
}

// postgresTestDSN creates a unique schema in the database at dsn and returns
// the DSN with search_path pointing at it, so migrations and all queries run
// isolated from other tests. The schema is dropped via t.Cleanup.
func postgresTestDSN(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("testutil: connect postgres: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("testutil: create schema %s: %v", schema, err)
	}
	_ = conn.Close(ctx)

	t.Cleanup(func() {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		_, _ = conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}

func SetupServer(t *testing.T) (*httptest.Server, *service.Service) {
	t.Helper()

	bm25Dir := t.TempDir()
	t.Setenv("RAGOTA_BM25_PATH", bm25Dir)
	// The webhook endpoint fails closed without a secret, and the server reads
	// it once at construction — so it has to be set before Build.
	t.Setenv("RAGOTA_WEBHOOK_SECRET", WebhookSecret)

	cfg := TestConfig(t)
	svc, err := setup.Build(context.Background(), cfg)
	if err != nil {
		t.Fatalf("setup build: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })

	srv := api.NewServer(svc, &cfg.Server)
	testSrv := httptest.NewServer(srv.Router())
	t.Cleanup(testSrv.Close)

	return testSrv, svc
}
