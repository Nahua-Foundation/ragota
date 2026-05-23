package qdrant

// HTTP-моки для Qdrant REST-клиента через httptest.Server. Покрывают:
//   - EnsureCollection: skip при существующей, create при 404, ошибка при non-200/404;
//   - collectionExists: 200/404/500;
//   - GetCollectionStats: happy/non-200;
//   - DeleteCollection: 200/404 (treated as success)/500;
//   - Upsert: no-op при пустых points, отправка body;
//   - DeleteByFilter, Search, Count: тело запроса и парсинг ответа;
//   - Ping: 200/non-200/network error.
// Внешних зависимостей нет.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// recordingHandler даёт удобный способ матчить путь+метод и считать хиты.
type route struct {
	method, path string
	handler      http.HandlerFunc
}

func mux(routes ...route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, rt := range routes {
			if rt.method == r.Method && rt.path == r.URL.Path {
				rt.handler(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})
}

func TestEnsureCollection_Existing(t *testing.T) {
	var puts int32
	srv := httptest.NewServer(mux(
		route{http.MethodGet, "/collections/c1", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":{"points_count":0}}`))
		}},
		route{http.MethodPut, "/collections/c1", func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&puts, 1)
			w.WriteHeader(http.StatusOK)
		}},
	))
	defer srv.Close()
	c := New(srv.URL)
	if err := c.EnsureCollection(context.Background(), "c1", 384, Cosine); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if atomic.LoadInt32(&puts) != 0 {
		t.Errorf("PUT must not be called for existing collection")
	}
}

func TestEnsureCollection_Create(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(mux(
		route{http.MethodGet, "/collections/new", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}},
		route{http.MethodPut, "/collections/new", func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			w.WriteHeader(http.StatusOK)
		}},
	))
	defer srv.Close()
	c := New(srv.URL)
	if err := c.EnsureCollection(context.Background(), "new", 768, Dot); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	vec, _ := body["vectors"].(map[string]any)
	if vec == nil || vec["distance"] != "Dot" {
		t.Errorf("bad PUT body: %+v", body)
	}
	// JSON-числа декодируются в float64.
	if size, _ := vec["size"].(float64); size != 768 {
		t.Errorf("bad size: %v", vec["size"])
	}
}

func TestCollectionExists_500Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(srv.URL)
	if _, err := c.collectionExists(context.Background(), "x"); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestGetCollectionStats(t *testing.T) {
	srv := httptest.NewServer(mux(
		route{http.MethodGet, "/collections/c", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":{"points_count":42}}`))
		}},
	))
	defer srv.Close()
	c := New(srv.URL)
	s, err := c.GetCollectionStats(context.Background(), "c")
	if err != nil {
		t.Fatalf("GetCollectionStats: %v", err)
	}
	if s.PointsCount != 42 {
		t.Errorf("got %d", s.PointsCount)
	}

	// non-200
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv2.Close()
	if _, err := New(srv2.URL).GetCollectionStats(context.Background(), "c"); err == nil {
		t.Error("expected error on 500")
	}
}

func TestDeleteCollection(t *testing.T) {
	// 200 OK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := New(srv.URL).DeleteCollection(context.Background(), "c"); err != nil {
		t.Errorf("delete 200: %v", err)
	}

	// 404 — также success (idempotent)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv2.Close()
	if err := New(srv2.URL).DeleteCollection(context.Background(), "c"); err != nil {
		t.Errorf("delete 404 must be nil: %v", err)
	}

	// 500 — ошибка
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv3.Close()
	if err := New(srv3.URL).DeleteCollection(context.Background(), "c"); err == nil {
		t.Error("expected error on 500")
	}
}

func TestUpsert_NoOpEmpty(t *testing.T) {
	var hit int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hit, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := New(srv.URL).Upsert(context.Background(), "c", nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if atomic.LoadInt32(&hit) != 0 {
		t.Error("Upsert(nil) must not hit server")
	}
}

func TestUpsert_SendsPoints(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(mux(
		route{http.MethodPut, "/collections/c/points", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "wait=true" {
				t.Errorf("expected wait=true, got %q", r.URL.RawQuery)
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &got)
			w.WriteHeader(http.StatusOK)
		}},
	))
	defer srv.Close()
	pts := []Point{{ID: "p1", Vector: []float32{1, 2}, Payload: map[string]any{"k": "v"}}}
	if err := New(srv.URL).Upsert(context.Background(), "c", pts); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	arr, _ := got["points"].([]any)
	if len(arr) != 1 {
		t.Fatalf("expected 1 point in body: %+v", got)
	}
}

func TestDeleteByFilter(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(mux(
		route{http.MethodPost, "/collections/c/points/delete", func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &got)
			w.WriteHeader(http.StatusOK)
		}},
	))
	defer srv.Close()
	if err := New(srv.URL).DeleteByFilter(context.Background(), "c", "path", "/x/y"); err != nil {
		t.Fatalf("DeleteByFilter: %v", err)
	}
	filter, _ := got["filter"].(map[string]any)
	if filter == nil {
		t.Fatalf("missing filter: %+v", got)
	}
	must, _ := filter["must"].([]any)
	if len(must) != 1 {
		t.Fatalf("must len: %v", must)
	}
}

func TestSearch(t *testing.T) {
	srv := httptest.NewServer(mux(
		route{http.MethodPost, "/collections/c/points/search", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":[{"id":"a","score":0.9,"payload":{"p":"x"}},{"id":"b","score":0.5}]}`))
		}},
	))
	defer srv.Close()
	hits, err := New(srv.URL).Search(context.Background(), "c", []float32{1, 0, 0}, 5, map[string]any{"must": []any{}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 || hits[0].ID != "a" || hits[0].Score != 0.9 {
		t.Errorf("hits: %+v", hits)
	}
}

func TestCount(t *testing.T) {
	srv := httptest.NewServer(mux(
		route{http.MethodPost, "/collections/c/points/count", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":{"count":17}}`))
		}},
	))
	defer srv.Close()
	n, err := New(srv.URL).Count(context.Background(), "c")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 17 {
		t.Errorf("got %d", n)
	}
}

func TestDo_ErrorBubblesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rejected", http.StatusBadRequest)
	}))
	defer srv.Close()
	err := New(srv.URL).Upsert(context.Background(), "c", []Point{{ID: "x", Vector: []float32{1}}})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err must contain status: %v", err)
	}
}

func TestPing(t *testing.T) {
	srv := httptest.NewServer(mux(
		route{http.MethodGet, "/readyz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}},
	))
	defer srv.Close()
	if err := New(srv.URL).Ping(context.Background()); err != nil {
		t.Errorf("Ping happy: %v", err)
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv2.Close()
	if err := New(srv2.URL).Ping(context.Background()); err == nil {
		t.Error("Ping must fail on 503")
	}

	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	addr := srv3.URL
	srv3.Close()
	if err := New(addr).Ping(context.Background()); err == nil {
		t.Error("Ping must fail on closed server")
	}
}
