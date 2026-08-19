package app

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	applyCommitsSeconds = promauto.NewSummary(prometheus.SummaryOpts{
		Name: "ragota_apply_commits_seconds",
		Help: "duration of a commit-application pass",
	})
	indexRepoSeconds = promauto.NewSummary(prometheus.SummaryOpts{
		Name: "ragota_index_repo_seconds",
		Help: "duration of a repository index pass",
	})
	indexUnreadableFiles = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_index_unreadable_files_total",
		Help: "files skipped because their content could not be read",
	})
	linkSeconds = promauto.NewSummary(prometheus.SummaryOpts{
		Name: "ragota_link_seconds",
		Help: "duration of an edge-linking pass",
	})
	linkErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_link_errors_total",
		Help: "edges that failed to link",
	})
	linkResolved = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_link_resolved_total",
		Help: "edges resolved to a target",
	})
)
