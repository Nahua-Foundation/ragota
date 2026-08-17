package qdrant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/httpx"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// Qdrant implements storage.VectorStorage with Qdrant backend.
type Qdrant struct {
	baseURL          string
	apiKey           string
	collectionPrefix string
	client           *httpx.Client

	// ready records that the collection has been checked (or created) with the
	// vector width this process embeds at, so the check is not repeated on
	// every upsert. See ensureCollection.
	readyMu sync.Mutex
	ready   map[string]int
}

// Config is the Qdrant storage configuration.
type Config struct {
	URL              string
	APIKey           string
	CollectionPrefix string
}

// Open creates a new Qdrant client.
func Open(cfg *Config) *Qdrant {
	q := &Qdrant{
		baseURL:          cfg.URL,
		apiKey:           cfg.APIKey,
		collectionPrefix: cfg.CollectionPrefix,
		ready:            map[string]int{},
		client: &httpx.Client{
			BaseURL: cfg.URL,
			HTTP: &http.Client{
				Timeout: 30 * time.Second,
				// The default transport keeps two idle connections per host,
				// so a store stage with more writers than that reconnects on
				// every call. Indexing is the only heavy user of this client
				// and it writes from a worker pool.
				Transport: pooledTransport(),
			},
		},
	}

	if q.apiKey != "" {
		q.client.Header = http.Header{
			"api-key": []string{q.apiKey},
		}
	}

	return q
}

// pooledTransport clones the default transport with room for the store stage's
// writers, so concurrent upserts reuse connections instead of paying a TCP
// handshake each.
func pooledTransport() http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	t = t.Clone()
	t.MaxIdleConnsPerHost = 32
	return t
}

// Init initializes the Qdrant storage.
func (q *Qdrant) Init(ctx context.Context) error {
	// Use collections endpoint instead of /health (deprecated)
	var response struct {
		Result struct {
			Collections []any `json:"collections"`
		} `json:"result"`
		Status string `json:"status"`
	}
	if err := q.client.GetJSON(ctx, "/collections", &response); err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	if response.Status != "ok" {
		return fmt.Errorf("unexpected status: %s", response.Status)
	}

	return nil
}

// collection returns the single collection name for all repos' chunks.
func (q *Qdrant) collection() string {
	if q.collectionPrefix != "" {
		return q.collectionPrefix + "chunks"
	}
	return "chunks"
}

// Close closes the Qdrant client.
func (q *Qdrant) Close() error {
	return nil
}

// Upsert inserts or updates vectors into the single chunks collection.
func (q *Qdrant) Upsert(ctx context.Context, points []*storage.VectorPoint) error {
	if len(points) == 0 {
		return nil
	}
	coll := q.collection()
	width := len(points[0].Vector)
	if err := q.ensureCollection(ctx, coll, width); err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}
	payload := map[string]interface{}{"points": q.buildPoints(points)}
	// Qdrant's point upsert is a PUT; a POST to the same path is a different
	// endpoint that expects an "ids" field and rejects the body.
	err := q.client.PutJSON(ctx, "/collections/"+coll+"/points", payload, nil)
	// A 404 means the collection went away after it was checked — it was
	// dropped by an operator, or by another process between the check and the
	// write. Forget what was cached about it and let one retry recreate it, so
	// the cache can never turn a recoverable state into a permanent failure.
	var httpErr *httpx.Error
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
		q.forget(coll)
		if err = q.ensureCollection(ctx, coll, width); err != nil {
			return fmt.Errorf("ensure collection: %w", err)
		}
		err = q.client.PutJSON(ctx, "/collections/"+coll+"/points", payload, nil)
	}
	if err != nil {
		return fmt.Errorf("upsert failed: %w", err)
	}
	return nil
}

// forget drops the cached readiness of a collection, so the next write checks
// it again.
func (q *Qdrant) forget(collectionName string) {
	q.readyMu.Lock()
	delete(q.ready, collectionName)
	q.readyMu.Unlock()
}

// buildPoints converts VectorPoints to Qdrant point format.
func (q *Qdrant) buildPoints(points []*storage.VectorPoint) []map[string]interface{} {
	result := make([]map[string]interface{}, len(points))

	for i, p := range points {
		payload := map[string]interface{}{
			"repo_id":    p.RepoID,
			"file_path":  p.FilePath,
			"language":   p.Language,
			"text":       p.Text,
			"start_line": p.StartLine,
			"end_line":   p.EndLine,
		}

		if p.Kind != "" {
			payload["kind"] = p.Kind
		}
		if p.Symbol != "" {
			payload["symbol"] = p.Symbol
		}
		for k, v := range p.Metadata {
			payload[k] = v
		}

		result[i] = map[string]interface{}{
			"id":      p.ID,
			"vector":  p.Vector,
			"payload": payload,
		}
	}

	return result
}

// collectionInfo is the part of Qdrant's collection description we need: the
// configured vector size of the default (unnamed) vector.
type collectionInfo struct {
	Result struct {
		Config struct {
			Params struct {
				Vectors struct {
					Size int `json:"size"`
				} `json:"vectors"`
			} `json:"params"`
		} `json:"config"`
	} `json:"result"`
}

// ensureCollection ensures a collection exists with the expected vector size,
// creating it if necessary.
//
// An existing collection is validated rather than trusted: changing the
// embedding model changes the vector width, and Qdrant would keep accepting
// writes into a collection whose stored vectors came from a different model,
// silently corrupting every later search. Failing loudly here is the only
// point where the mismatch is still visible.
//
// The answer is cached per collection and vector width, because the caller is
// Upsert and Upsert is called once per file: the check cost four round trips
// per file — a GET plus one payload-index PUT per indexed field, each of which
// Qdrant answers with 200 and a few milliseconds of work rather than the
// conflict the code expected. On Elasticsearch that is four wasted requests
// for every one that carries data, in the stage the whole pipeline waits on.
// A width that changes mid-process still goes through the full check, so the
// model-changed error cannot be cached away.
func (q *Qdrant) ensureCollection(ctx context.Context, collectionName string, vectorSize int) error {
	q.readyMu.Lock()
	known, ok := q.ready[collectionName]
	q.readyMu.Unlock()
	if ok && known == vectorSize {
		return nil
	}

	if err := q.checkCollection(ctx, collectionName, vectorSize); err != nil {
		return err
	}

	q.readyMu.Lock()
	q.ready[collectionName] = vectorSize
	q.readyMu.Unlock()
	return nil
}

// checkCollection performs the uncached existence and width check.
func (q *Qdrant) checkCollection(ctx context.Context, collectionName string, vectorSize int) error {
	var info collectionInfo
	err := q.client.GetJSON(ctx, "/collections/"+collectionName, &info)
	if err == nil {
		got := info.Result.Config.Params.Vectors.Size
		if got != 0 && vectorSize != 0 && got != vectorSize {
			return fmt.Errorf(
				"collection %s has vector size %d but the embedder produces %d: "+
					"the embedding model changed; recreate the collection or restore the previous model",
				collectionName, got, vectorSize)
		}
		// Pre-existing collections (older deployments) may lack the payload
		// indexes; creating them is idempotent and cheap.
		return q.ensurePayloadIndexes(ctx, collectionName)
	}

	var httpErr *httpx.Error
	if errors.As(err, &httpErr) && httpErr.Status == 404 {
		return q.createCollection(ctx, collectionName, vectorSize)
	}

	return fmt.Errorf("check collection failed: %w", err)
}

// createCollection creates a new collection.
func (q *Qdrant) createCollection(ctx context.Context, collectionName string, vectorSize int) error {
	payload := map[string]interface{}{
		"vectors": map[string]interface{}{
			"size":     vectorSize,
			"distance": "Cosine",
		},
	}

	// Collection creation is a PUT in the Qdrant REST API; POST returns 404.
	err := q.client.PutJSON(ctx, "/collections/"+collectionName, payload, nil)
	// Repos are indexed in parallel, so several workers can pass the existence
	// check together and race to create the shared collection. The loser's 409
	// means the collection is there, which is all the caller needs.
	var httpErr *httpx.Error
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict {
		return q.ensurePayloadIndexes(ctx, collectionName)
	}
	if err != nil {
		return fmt.Errorf("create collection failed: %w", err)
	}

	return q.ensurePayloadIndexes(ctx, collectionName)
}

// indexedPayloadFields are the payload fields every filtered operation uses:
// deletes select by repo_id + file_path (a file's points are replaced on
// every re-index), searches filter by repo_id and language.
var indexedPayloadFields = []string{"repo_id", "file_path", "language"}

// ensurePayloadIndexes creates keyword payload indexes for the fields the
// store filters on. Without them Qdrant answers a filtered delete or search
// with a full scan — measured on a 120k-point collection, the per-file
// delete-before-upsert became the bottleneck of the whole indexing pipeline
// (quadratic in corpus size) and one CPU core went entirely to scanning.
//
// Creation is idempotent, so racing workers and re-opens are harmless — but
// not free: Qdrant answers a re-creation with 200 and several milliseconds of
// work, not the conflict the status handling below expects, which is why
// ensureCollection caches rather than calling this per write.
func (q *Qdrant) ensurePayloadIndexes(ctx context.Context, collectionName string) error {
	for _, field := range indexedPayloadFields {
		payload := map[string]interface{}{
			"field_name":   field,
			"field_schema": "keyword",
		}
		err := q.client.PutJSON(ctx, "/collections/"+collectionName+"/index", payload, nil)
		var httpErr *httpx.Error
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict {
			continue // already indexed
		}
		// A 400 used to be swallowed here too, which hid a genuinely malformed
		// index request (and, since payload indexes gate filtered deletes, let
		// them silently degrade to full scans). Only Qdrant's "already exists"
		// (409) is benign; anything else is a real error.
		if err != nil {
			return fmt.Errorf("create payload index %s: %w", field, err)
		}
	}
	return nil
}

// Search performs vector similarity search.
func (q *Qdrant) Search(ctx context.Context, opts storage.VectorSearchOpts) ([]*storage.VectorResult, error) {
	collectionName := q.collection()

	payload := map[string]interface{}{
		"vector":       opts.Query,
		"limit":        opts.Limit,
		"with_payload": true,
	}

	// Repo, language and payload filters all go into one "must" clause;
	// building them separately would let the later assignment drop the earlier.
	must := q.buildFilters(opts.Filter)
	if len(opts.Repos) > 0 {
		must = append(must, map[string]interface{}{
			"key":   "repo_id",
			"match": map[string]interface{}{"any": opts.Repos},
		})
	}
	if len(opts.Languages) > 0 {
		must = append(must, map[string]interface{}{
			"key":   "language",
			"match": map[string]interface{}{"any": opts.Languages},
		})
	}
	if len(must) > 0 {
		payload["filter"] = map[string]interface{}{"must": must}
	}

	var response struct {
		Result []struct {
			ID      any            `json:"id"`
			Score   float32        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}

	if err := q.client.PostJSON(ctx, "/collections/"+collectionName+"/points/search", payload, &response); err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	results := make([]*storage.VectorResult, len(response.Result))
	for i, r := range response.Result {
		results[i] = &storage.VectorResult{
			ID:       fmt.Sprintf("%v", r.ID),
			Score:    r.Score,
			RepoID:   getString(r.Payload, "repo_id"),
			FilePath: getString(r.Payload, "file_path"),
			EndLine:  getInt(r.Payload, "end_line"),
			Line:     getInt(r.Payload, "start_line"),
			Text:     getString(r.Payload, "text"),
			Metadata: make(storage.Metadata),
		}

		// Copy metadata
		for k, v := range r.Payload {
			if s, ok := v.(string); ok {
				results[i].Metadata[k] = s
			}
		}
	}

	return results, nil
}

// buildFilters converts filter map to Qdrant filter format.
func (q *Qdrant) buildFilters(filter map[string]string) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(filter))

	for key, value := range filter {
		result = append(result, map[string]interface{}{
			"key": key,
			"match": map[string]interface{}{
				"value": value,
			},
		})
	}

	return result
}

// Delete removes vectors by repo and/or file.
func (q *Qdrant) Delete(ctx context.Context, repoID, filePath string) error {
	collectionName := q.collection()

	// Build filter
	filter := map[string]interface{}{
		"must": []map[string]interface{}{
			{"key": "repo_id", "match": map[string]interface{}{"value": repoID}},
		},
	}

	if filePath != "" {
		filter["must"] = append(filter["must"].([]map[string]interface{}), map[string]interface{}{
			"key": "file_path", "match": map[string]interface{}{"value": filePath},
		})
	}

	payload := map[string]interface{}{
		"filter": filter,
	}

	err := q.client.PostJSON(ctx, "/collections/"+collectionName+"/points/delete", payload, nil)
	// Indexing deletes a file's previous points before writing the new ones, so
	// on a fresh deployment the very first delete precedes the collection. A
	// missing collection holds nothing to delete — that is success, not an
	// error that should fail the file.
	var httpErr *httpx.Error
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete failed: %w", err)
	}

	return nil
}

// Stats returns vector storage statistics.
func (q *Qdrant) Stats(ctx context.Context) (*storage.VectorStats, error) {
	// The collection list carries names and nothing else — the counts live on
	// each collection's own endpoint. Reading them off the list is what made
	// this report zeroes: the fields were declared, the JSON never had them,
	// and /stats said the vector index held no documents however much it held.
	var list struct {
		Result struct {
			Collections []struct {
				Name string `json:"name"`
			} `json:"collections"`
		} `json:"result"`
	}
	if err := q.client.GetJSON(ctx, "/collections", &list); err != nil {
		return nil, fmt.Errorf("list collections failed: %w", err)
	}

	var totalPoints int64
	collections := 0
	for _, col := range list.Result.Collections {
		if q.collectionPrefix != "" && !strings.HasPrefix(col.Name, q.collectionPrefix) {
			continue
		}
		collections++

		var info struct {
			Result struct {
				PointsCount int64 `json:"points_count"`
			} `json:"result"`
		}
		if err := q.client.GetJSON(ctx, "/collections/"+col.Name, &info); err != nil {
			// One unreadable collection must not blank the whole report: the
			// others still describe what is stored.
			slog.Warn("qdrant collection info", "collection", col.Name, "err", err)
			continue
		}
		totalPoints += info.Result.PointsCount
	}

	return &storage.VectorStats{
		Documents: totalPoints,
		// Qdrant reports no on-disk size for a collection, so this stays zero
		// rather than carrying a number that would be invented.
		SizeBytes: 0,
		// Every repository shares one collection, so this counts the
		// collections this instance owns, not the repositories in them.
		Repos:      collections,
		Collection: q.collectionPrefix,
	}, nil
}

// Helper functions

func getString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getInt(m map[string]any, key string) int {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return 0
}
