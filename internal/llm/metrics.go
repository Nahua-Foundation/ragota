package llm

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var rerankRequestFailures = promauto.NewCounter(prometheus.CounterOpts{
	Name: "ragota_rerank_request_failures_total",
	Help: "reranker HTTP requests that failed after retries",
})
