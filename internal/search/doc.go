// Package search provides search and result fusion services.
//
// Files:
//   - hybrid.go: Hybrid search with RRF, weighted, and concat fusion methods
//
// The search service combines results from multiple indexers (vector, BM25)
// using configurable fusion methods.
package search
