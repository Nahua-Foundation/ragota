package qdrant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== Constructor ====================

func TestNew_TrimsTrailingSlash(t *testing.T) {
	c := New("http://localhost:6333/")
	assert.Equal(t, "http://localhost:6333", c.baseURL)
}

func TestNew_NoTrailingSlash(t *testing.T) {
	c := New("http://localhost:6333")
	assert.Equal(t, "http://localhost:6333", c.baseURL)
}

// ==================== Search edge cases ====================

func TestSearch_NoFilter(t *testing.T) {
	srv := httptest.NewServer(mux(
		route{http.MethodPost, "/collections/c/points/search", func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			// filter should not be present
			if _, ok := req["filter"]; ok {
				t.Error("filter should not be in request when nil")
			}
			_, _ = w.Write([]byte(`{"result":[]}`))
		}},
	))
	defer srv.Close()

	hits, err := New(srv.URL).Search(context.Background(), "c", []float32{1}, 10, nil)
	require.NoError(t, err)
	assert.Empty(t, hits)
}

func TestSearch_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(mux(
		route{http.MethodPost, "/collections/c/points/search", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":[]}`))
		}},
	))
	defer srv.Close()

	hits, err := New(srv.URL).Search(context.Background(), "c", []float32{1, 0}, 5, nil)
	require.NoError(t, err)
	assert.Empty(t, hits)
}

func TestSearch_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := New(srv.URL).Search(context.Background(), "c", []float32{1}, 5, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestSearch_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(mux(
		route{http.MethodPost, "/collections/c/points/search", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}},
	))
	defer srv.Close()

	_, err := New(srv.URL).Search(context.Background(), "c", []float32{1}, 5, nil)
	assert.Error(t, err)
}

// ==================== Count edge cases ====================

func TestCount_Zero(t *testing.T) {
	srv := httptest.NewServer(mux(
		route{http.MethodPost, "/collections/c/points/count", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":{"count":0}}`))
		}},
	))
	defer srv.Close()

	n, err := New(srv.URL).Count(context.Background(), "c")
	require.NoError(t, err)
	assert.Equal(t, uint64(0), n)
}

func TestCount_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "error", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := New(srv.URL).Count(context.Background(), "c")
	assert.Error(t, err)
}

// ==================== DeleteByFilter edge cases ====================

func TestDeleteByFilter_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := New(srv.URL).DeleteByFilter(context.Background(), "c", "path", "/foo")
	assert.Error(t, err)
}

func TestDeleteByFilter_NumericValue(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(mux(
		route{http.MethodPost, "/collections/c/points/delete", func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &got)
			w.WriteHeader(http.StatusOK)
		}},
	))
	defer srv.Close()

	err := New(srv.URL).DeleteByFilter(context.Background(), "c", "age", 25)
	require.NoError(t, err)
	assert.NotNil(t, got["filter"])
}

// ==================== Upsert edge cases ====================

func TestUpsert_MultiplePoints(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(mux(
		route{http.MethodPut, "/collections/c/points", func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &got)
			w.WriteHeader(http.StatusOK)
		}},
	))
	defer srv.Close()

	pts := []Point{
		{ID: "p1", Vector: []float32{1, 2}},
		{ID: "p2", Vector: []float32{3, 4}},
		{ID: "p3", Vector: []float32{5, 6}},
	}
	err := New(srv.URL).Upsert(context.Background(), "c", pts)
	require.NoError(t, err)
	arr, _ := got["points"].([]any)
	assert.Len(t, arr, 3)
}

// ==================== EnsureCollection edge cases ====================

func TestEnsureCollection_GetError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := New(srv.URL).EnsureCollection(context.Background(), "c", 384, Cosine)
	assert.Error(t, err)
}

func TestEnsureCollection_AllDistances(t *testing.T) {
	for _, dist := range []Distance{Cosine, Euclidean, Dot} {
		t.Run(string(dist), func(t *testing.T) {
			srv := httptest.NewServer(mux(
				route{http.MethodGet, "/collections/c", func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNotFound)
				}},
				route{http.MethodPut, "/collections/c", func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					var body map[string]any
					_ = json.Unmarshal(b, &body)
					vecs := body["vectors"].(map[string]any)
					if vecs["distance"] != string(dist) {
						t.Errorf("expected %s, got %v", dist, vecs["distance"])
					}
					w.WriteHeader(http.StatusOK)
				}},
			))
			defer srv.Close()

			err := New(srv.URL).EnsureCollection(context.Background(), "c", 384, dist)
			require.NoError(t, err)
		})
	}
}

// ==================== GetCollectionStats edge cases ====================

func TestGetCollectionStats_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := New(srv.URL).GetCollectionStats(context.Background(), "c")
	assert.Error(t, err)
}

// ==================== DeleteCollection edge cases ====================

func TestDeleteCollection_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := New(srv.URL).DeleteCollection(context.Background(), "c")
	assert.Error(t, err)
}

// ==================== do helper edge cases ====================

func TestDo_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	addr := srv.URL
	srv.Close()

	c := New(addr)
	err := c.Ping(context.Background())
	assert.Error(t, err)
}

// ==================== Distance constants ====================

func TestDistanceConstants(t *testing.T) {
	assert.Equal(t, Distance("Cosine"), Cosine)
	assert.Equal(t, Distance("Euclid"), Euclidean)
	assert.Equal(t, Distance("Dot"), Dot)
}
