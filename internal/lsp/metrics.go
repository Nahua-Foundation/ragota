package lsp

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	lspPassTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_pass_total",
		Help: "LSP refinement passes started",
	})
	lspServerFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_server_failures",
		Help: "language skipped: dial/initialize failed",
	})
	lspEmptyLanguages = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_empty_languages",
		Help: "language yielded no symbols at all",
	})
	lspFilesRefined = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_files_refined",
		Help: "files successfully refined",
	})
	lspFilesFailed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_files_failed",
		Help: "per-file refinement errors",
	})
	lspFilesSkipped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_files_skipped",
		Help: "no server configured for the language",
	})
	lspReferenceErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_reference_errors",
		Help: "textDocument/references failed",
	})
	lspDialChecks = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_dial_checks",
		Help: "startup reachability probes",
	})
	lspDialFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_dial_failures",
		Help: "startup reachability probes that failed",
	})
	lspPassSeconds = promauto.NewSummary(prometheus.SummaryOpts{
		Name: "ragota_lsp_pass_seconds",
		Help: "duration of a refinement pass",
	})
	lspCallPassSeconds = promauto.NewSummary(prometheus.SummaryOpts{
		Name: "ragota_lsp_call_pass_seconds",
		Help: "duration of a call-edge correction pass",
	})
	lspCallRequests = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_call_requests",
		Help: "textDocument/references requests issued",
	})
	lspCallConfirmed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_call_confirmed",
		Help: "call edges confirmed at their existing target",
	})
	lspCallRepointed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_call_repointed",
		Help: "call edges moved to another definition",
	})
	lspCallAdded = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_call_added",
		Help: "call edges the tree-sitter pass had missed",
	})
	lspCallContradicted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_call_contradicted",
		Help: "resolutions the language server denies",
	})
	lspCallTruncated = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ragota_lsp_call_truncated",
		Help: "repos where the symbol budget ran out",
	})
)
