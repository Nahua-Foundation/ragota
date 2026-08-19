package enrich

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var reconTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "ragota_recon_total",
	Help: "recon passes run",
})
