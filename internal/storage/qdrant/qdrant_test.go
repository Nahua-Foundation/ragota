package qdrant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// collectionServer serves a Qdrant-shaped collection description with the
// given vector size, or 404 when size is 0 (collection does not exist).
func collectionServer(t *testing.T, size int, created *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/collections/"):
			if size == 0 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"status":"not found"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{
					"config": map[string]any{
						"params": map[string]any{
							"vectors": map[string]any{"size": size, "distance": "Cosine"},
						},
					},
				},
				"status": "ok",
			})
		case r.Method == http.MethodPut && !strings.Contains(r.URL.Path, "/points"):
			if created != nil {
				*created = true
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":true,"status":"ok"}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{},"status":"ok"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEnsureCollectionRejectsDimensionMismatch covers the silent corruption
// path: swapping the embedding model changes the vector width, and an existing
// collection would keep accepting writes of the wrong size.
func TestEnsureCollectionRejectsDimensionMismatch(t *testing.T) {
	srv := collectionServer(t, 768, nil)
	q := Open(&Config{URL: srv.URL})

	err := q.ensureCollection(context.Background(), "chunks", 1536)
	if err == nil {
		t.Fatal("ensureCollection() = nil, want an error for a vector size mismatch")
	}
	for _, want := range []string{"768", "1536"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestEnsureCollectionAcceptsMatchingDimension(t *testing.T) {
	srv := collectionServer(t, 768, nil)
	q := Open(&Config{URL: srv.URL})

	if err := q.ensureCollection(context.Background(), "chunks", 768); err != nil {
		t.Fatalf("ensureCollection() with a matching size = %v, want nil", err)
	}
}

func TestEnsureCollectionCreatesWhenMissing(t *testing.T) {
	created := false
	srv := collectionServer(t, 0, &created)
	q := Open(&Config{URL: srv.URL})

	if err := q.ensureCollection(context.Background(), "chunks", 384); err != nil {
		t.Fatalf("ensureCollection() = %v, want nil", err)
	}
	if !created {
		t.Error("collection was not created")
	}
}

// TestUpsertRejectsDimensionMismatch checks the validation is reached on the
// write path, which is where a wrong-width vector would land.
func TestUpsertRejectsDimensionMismatch(t *testing.T) {
	srv := collectionServer(t, 4, nil)
	q := Open(&Config{URL: srv.URL})

	err := q.Upsert(context.Background(), []*storage.VectorPoint{
		{ID: "1", Vector: []float32{1, 2}},
	})
	if err == nil {
		t.Fatal("Upsert() = nil, want an error for a vector size mismatch")
	}
}

// countingServer serves the same shapes as collectionServer and tallies the
// requests by kind, so a test can assert how many round trips one upsert costs.
type countingServer struct {
	*httptest.Server
	mu      sync.Mutex
	get     int // GET /collections/<name>
	index   int // PUT /collections/<name>/index
	points  int // PUT /collections/<name>/points
	missing bool
}

func newCountingServer(t *testing.T, size int) *countingServer {
	t.Helper()
	c := &countingServer{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		switch {
		case r.Method == http.MethodGet:
			c.get++
		case strings.HasSuffix(r.URL.Path, "/index"):
			c.index++
		case strings.HasSuffix(r.URL.Path, "/points"):
			c.points++
		}
		missing := c.missing
		c.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			if missing {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"status":"not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{"config": map[string]any{"params": map[string]any{
					"vectors": map[string]any{"size": size, "distance": "Cosine"},
				}}},
				"status": "ok",
			})
		case missing && strings.HasSuffix(r.URL.Path, "/points"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":"not found"}`))
		default:
			_, _ = w.Write([]byte(`{"result":true,"status":"ok"}`))
		}
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *countingServer) counts() (get, index, points int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.get, c.index, c.points
}

// TestUpsertChecksCollectionOnce pins the cost of a write. Indexing calls
// Upsert once per file, and the collection check behind it used to cost a GET
// plus one payload-index PUT per indexed field on every one of them — four
// round trips of pure overhead for each one that carried data, in the stage
// the whole pipeline waits on.
func TestUpsertChecksCollectionOnce(t *testing.T) {
	srv := newCountingServer(t, 4)
	q := Open(&Config{URL: srv.URL})

	for range 5 {
		if err := q.Upsert(context.Background(), []*storage.VectorPoint{
			{ID: "1", Vector: []float32{1, 2, 3, 4}},
		}); err != nil {
			t.Fatalf("Upsert() = %v, want nil", err)
		}
	}

	get, index, points := srv.counts()
	if points != 5 {
		t.Errorf("point writes = %d, want 5", points)
	}
	if get != 1 {
		t.Errorf("collection lookups = %d, want 1 for five upserts", get)
	}
	if index != len(indexedPayloadFields) {
		t.Errorf("payload index writes = %d, want %d for five upserts", index, len(indexedPayloadFields))
	}
}

// TestUpsertRecoversFromDroppedCollection is the cache's escape hatch: a
// collection that disappears after it was checked must be recreated, not
// remembered as ready forever.
func TestUpsertRecoversFromDroppedCollection(t *testing.T) {
	srv := newCountingServer(t, 4)
	q := Open(&Config{URL: srv.URL})

	point := []*storage.VectorPoint{{ID: "1", Vector: []float32{1, 2, 3, 4}}}
	if err := q.Upsert(context.Background(), point); err != nil {
		t.Fatalf("first Upsert() = %v, want nil", err)
	}

	// The collection goes away, so the next write 404s and has to recreate it.
	srv.mu.Lock()
	srv.missing = true
	srv.mu.Unlock()
	if err := q.Upsert(context.Background(), point); err == nil {
		t.Fatal("Upsert() into a vanished collection = nil, want the write error")
	}

	srv.mu.Lock()
	srv.missing = false
	srv.mu.Unlock()
	if err := q.Upsert(context.Background(), point); err != nil {
		t.Fatalf("Upsert() after the collection came back = %v, want nil", err)
	}
	if get, _, _ := srv.counts(); get < 2 {
		t.Errorf("collection lookups = %d, want the 404 to have forced a re-check", get)
	}
}

// TestDeleteOnMissingCollection pins the fresh-deployment path: indexing
// deletes a file's old points before upserting, so the first delete of a new
// deployment hits a collection that does not exist yet. That must not fail the
// file — otherwise nothing is ever written and the vector index stays empty.
func TestDeleteOnMissingCollection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":{"error":"Not found: Collection ` + "`ragota_chunks`" + ` doesn't exist!"}}`))
	}))
	defer srv.Close()

	q := Open(&Config{URL: srv.URL, CollectionPrefix: "ragota_"})
	if err := q.Delete(context.Background(), "repo1", "main.go"); err != nil {
		t.Fatalf("Delete() on a missing collection = %v, want nil", err)
	}
}
