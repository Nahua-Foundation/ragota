// Package qdrant — минимальный REST-клиент Qdrant.
// Покрывает только нужные операции: create/recreate collection, upsert,
// delete by filter, search.
package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client — REST-клиент Qdrant.
type Client struct {
	baseURL string
	http    *http.Client
}

// New создаёт клиента. baseURL вида "http://localhost:6333".
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Distance — метрика близости.
type Distance string

const (
	Cosine    Distance = "Cosine"
	Euclidean Distance = "Euclid"
	Dot       Distance = "Dot"
)

// Point — точка для upsert.
type Point struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload,omitempty"`
}

// SearchHit — результат поиска.
type SearchHit struct {
	ID      string         `json:"id"`
	Score   float32        `json:"score"`
	Payload map[string]any `json:"payload,omitempty"`
}

// EnsureCollection создаёт коллекцию с указанной размерностью, если её нет.
func (c *Client) EnsureCollection(ctx context.Context, name string, dim uint64, dist Distance) error {
	// Сначала проверяем существование.
	exists, err := c.collectionExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	body := map[string]any{
		"vectors": map[string]any{
			"size":     dim,
			"distance": string(dist),
		},
	}
	return c.doPUT(ctx, "/collections/"+name, body, nil)
}

func (c *Client) collectionExists(ctx context.Context, name string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/collections/"+name, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	buf, _ := io.ReadAll(resp.Body)
	return false, fmt.Errorf("qdrant collectionExists status %d: %s", resp.StatusCode, string(buf))
}

// CollectionStats возвращает статистику коллекции (кол-во точек).
type CollectionStats struct {
	PointsCount int `json:"points_count"`
}

func (c *Client) GetCollectionStats(ctx context.Context, name string) (*CollectionStats, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/collections/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("qdrant stats status %d: %s", resp.StatusCode, string(buf))
	}
	var res struct {
		Result struct {
			PointsCount int `json:"points_count"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &CollectionStats{PointsCount: res.Result.PointsCount}, nil
}

// DeleteCollection удаляет коллекцию целиком. Если её нет — возвращает nil.
func (c *Client) DeleteCollection(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/collections/"+name, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("qdrant DELETE collection: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode < 300 {
		return nil
	}
	buf, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("qdrant delete collection status %d: %s", resp.StatusCode, string(buf))
}

// Upsert загружает точки в коллекцию.
func (c *Client) Upsert(ctx context.Context, collection string, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	body := map[string]any{"points": points}
	return c.doPUT(ctx, "/collections/"+collection+"/points?wait=true", body, nil)
}

// DeleteByFilter удаляет точки, у которых payload-поле key равно value.
func (c *Client) DeleteByFilter(ctx context.Context, collection, key string, value any) error {
	body := map[string]any{
		"filter": map[string]any{
			"must": []map[string]any{
				{"key": key, "match": map[string]any{"value": value}},
			},
		},
	}
	return c.doPOST(ctx, "/collections/"+collection+"/points/delete?wait=true", body, nil)
}

// Search ищет K ближайших соседей с опциональным filter.
func (c *Client) Search(ctx context.Context, collection string, vector []float32, limit int, filter map[string]any) ([]SearchHit, error) {
	body := map[string]any{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
	}
	if filter != nil {
		body["filter"] = filter
	}
	var out struct {
		Result []SearchHit `json:"result"`
	}
	if err := c.doPOST(ctx, "/collections/"+collection+"/points/search", body, &out); err != nil {
		return nil, err
	}
	return out.Result, nil
}

// Count возвращает примерное число точек в коллекции.
func (c *Client) Count(ctx context.Context, collection string) (uint64, error) {
	body := map[string]any{"exact": false}
	var out struct {
		Result struct {
			Count uint64 `json:"count"`
		} `json:"result"`
	}
	if err := c.doPOST(ctx, "/collections/"+collection+"/points/count", body, &out); err != nil {
		return 0, err
	}
	return out.Result.Count, nil
}

func (c *Client) doPUT(ctx context.Context, path string, body any, out any) error {
	return c.do(ctx, http.MethodPut, path, body, out)
}

func (c *Client) doPOST(ctx context.Context, path string, body any, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("qdrant %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("qdrant %s %s status %d: %s", method, path, resp.StatusCode, string(buf))
	}
	if out != nil && len(buf) > 0 {
		if err := json.Unmarshal(buf, out); err != nil {
			return fmt.Errorf("qdrant decode: %w", err)
		}
	}
	return nil
}

// Ping проверяет доступность Qdrant.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qdrant readyz status %d", resp.StatusCode)
	}
	return nil
}
