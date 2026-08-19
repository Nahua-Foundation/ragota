package search

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	searcherFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_search_searcher_failures_total",
		Help: "indexer searches that errored during hybrid fusion",
	})
	rerankSeconds = promauto.NewSummary(prometheus.SummaryOpts{
		Name: "ragota_rerank_seconds",
		Help: "duration of a rerank pass",
	})
	rerankFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_rerank_failures_total",
		Help: "rerank passes that failed",
	})
)
