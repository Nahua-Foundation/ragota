package bootstrap

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	configWarnings = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_config_warnings",
		Help: "suspicious-but-valid config settings",
	})
	embedderInitFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_embedder_init_failures",
		Help: "embedder could not be constructed",
	})
	parserSkippedLangs = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_ast_languages_disabled",
		Help: "parsers not registered by config",
	})
	gitAuthEnvFallback = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_git_auth_env_fallback",
		Help: "git token taken from the environment",
	})
)
