package bm25

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	symbolsReused = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_bm25_symbols_reused_total",
		Help: "files annotated from already-published symbols",
	})
	symbolsParsed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_bm25_symbols_parsed_total",
		Help: "files annotated from a local parse",
	})
	compactSeconds = promauto.NewSummary(prometheus.SummaryOpts{
		Name: "ragota_bm25_compact_seconds",
		Help: "duration of an index compaction",
	})
	compactUnsettled = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_bm25_compact_unsettled_total",
		Help: "compactions skipped because the index was not settled",
	})
	searchPanics = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_bm25_search_panics_total",
		Help: "searches recovered from an index panic",
	})
	searchDamaged = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_bm25_search_damaged_total",
		Help: "searches that hit a damaged index",
	})
)
