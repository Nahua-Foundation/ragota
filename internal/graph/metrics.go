package graph

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var disambigTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "ragota_disambig_total",
	Help: "edges resolved through ambiguity disambiguation",
})
