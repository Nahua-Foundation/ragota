package vector

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"ragota/pkg/config"
	"ragota/internal/store"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Root = t.TempDir()
	return cfg
}

func openTestStore(t *testing.T) *store.SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mockQdrantServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/collections/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/collections/"), "/")
		collName := parts[0]
		switch {
		case r.Method == http.MethodGet && len(parts) == 1:
			fmt.Fprintf(w, `{"result":{"status":"green","points_count":0,"vectors_count":0,"config":{"params":{"vectors":{"size":1024,"distance":"Cosine"}}}}}`)
		case r.Method == http.MethodPut && len(parts) == 1:
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":true,"status":"ok","time":0.001}`)
		case r.Method == http.MethodDelete && len(parts) == 1:
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":true,"status":"ok","time":0.001}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":{"operation_id":1,"status":"completed"},"status":"ok","time":0.001}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/delete"):
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":{"operation_id":2,"status":"completed"},"status":"ok","time":0.001}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/search"):
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/count"):
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"result":{"count":0}}`)
		default:
			_ = collName
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func mockOllamaServer(t *testing.T, dim int) *httptest.Server {
	t.Helper()
	if dim <= 0 {
		dim = 4
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"models":[]}`)
	})
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		vec := make([]string, dim)
		for i := range vec {
			vec[i] = "0.1"
		}
		vecJSON := strings.Join(vec, ",")
		fmt.Fprintf(w, `{"embeddings":[[%s]]}`, vecJSON)
	})
	mux.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		vec := make([]string, dim)
		for i := range vec {
			vec[i] = "0.1"
		}
		vecJSON := strings.Join(vec, ",")
		fmt.Fprintf(w, `{"embedding":[%s]}`, vecJSON)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// mockOllamaServerMultiBatch returns dim vectors for batch requests.
func mockOllamaServerMultiBatch(t *testing.T, dim int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"models":[]}`)
	})
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		vec := make([]string, dim)
		for j := range vec {
			vec[j] = "0.1"
		}
		vecs := "[" + strings.Join(vec, ",") + "]"
		fmt.Fprintf(w, `{"embeddings":[%s]}`, vecs)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
