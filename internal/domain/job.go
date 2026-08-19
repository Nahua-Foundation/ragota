package domain

// Index job statuses.
const (
	JobStatusPending = "pending"
	JobStatusRunning = "running"
	JobStatusDone    = "done"
	JobStatusError   = "error"
)

// Index job kinds. They share one queue (and one worker loop) but differ in
// what a job carries and how many of them may be queued per repo: an index job
// is a repeatable "reindex this repo" request, of which one pending entry per
// repo is enough, while a commit job carries a specific batch of commits that
// must be applied exactly once and in order.
const (
	JobKindIndex   = "index"
	JobKindCommits = "commits"
)

// IndexJob is a queued repository task. In distributed mode several
// ragota instances share one queue over a common database and claim jobs
// atomically.
type IndexJob struct {
	ID          string `json:"id"`
	RepoID      string `json:"repo_id"`
	Kind        string `json:"kind"` // index | commits
	Force       bool   `json:"force"`
	Status      string `json:"status"` // pending | running | done | error
	Error       string `json:"error,omitempty"`
	CreatedAt   int64  `json:"created_at"`   // unix seconds
	ClaimedAt   int64  `json:"claimed_at"`   // unix seconds, 0 if never claimed
	HeartbeatAt int64  `json:"heartbeat_at"` // unix seconds, 0 if never claimed
	ClaimedBy   string `json:"claimed_by,omitempty"`
	// Payload is the opaque body of a commit job (the encoded commit batch).
	// It is only populated by ClaimNextIndexJob: it can be tens of megabytes,
	// so the read paths that merely report queue state never select it, and it
	// is never serialized to API clients.
	Payload string `json:"-"`
}

// Contract kinds reported by coverage. They are the families of outbound
// contract an indexer can recognize at a call site; every coverage counter is
// keyed by one of them.
const (
	ContractKindHTTP      = "http"      // outbound HTTP request
	ContractKindRPC       = "rpc"       // gRPC / RPC client call
	ContractKindMessaging = "messaging" // publish/subscribe on a broker
	ContractKindDB        = "db"        // query against a table
)

// ContractKinds lists the coverage kinds in report order.
var ContractKinds = []string{ContractKindHTTP, ContractKindRPC, ContractKindMessaging, ContractKindDB}

// CoverageCounts is one contract kind's tally for a repository: how many call
// sites looked like an outbound contract, and how many of them produced an
// edge.
//
// The gap between the two is the only thing that separates "there is nothing
// to find here" from "we did not find it": a CMS with 42 HTTP calls and 42
// candidates is fully covered, while a project with 104 edges against
// thousands of candidates is not — the counts are equal in the first case and
// nowhere near it in the second.
type CoverageCounts struct {
	Candidates int `json:"candidates"`
	Edges      int `json:"edges"`
}

// RepoCoverage is the per-repo contract coverage summary written by the last
// full index pass.
type RepoCoverage struct {
	RepoID    string                    `json:"repo_id"`
	UpdatedAt int64                     `json:"updated_at"` // unix seconds
	Kinds     map[string]CoverageCounts `json:"kinds"`
}
