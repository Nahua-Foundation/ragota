package api

import (
	"encoding/json"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/graph"
	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/service"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// Mapping from the internal types onto the wire types in wire.go.
//
// These exist because the alternative was serializing the storage rows and
// domain structs themselves, which made the database schema the HTTP contract:
// a client saw start_byte, end_byte, the content hash and a raw JSON meta
// string, and a field added for the indexer's own use appeared in responses
// without anyone deciding it should (edge meta grew a base_conf for the linker
// and clients started receiving it). What a caller gets is chosen here.

func toSearchHit(h *indexing.Hit, snippet string) *SearchHit {
	if h == nil {
		return nil
	}
	return &SearchHit{
		RepoID: h.RepoID, FilePath: h.FilePath,
		Line: h.Line, EndLine: h.EndLine,
		Symbol: h.Symbol, Kind: h.Kind, Language: h.Language,
		Score: h.Score, Snippet: renderSnippet(h.Snippet, snippet), Reason: h.Reason,
	}
}

func toSearchHits(in []*indexing.Hit, snippet string) []*SearchHit {
	out := make([]*SearchHit, 0, len(in))
	for _, h := range in {
		out = append(out, toSearchHit(h, snippet))
	}
	return out
}

// renderSnippet cuts a hit's code body down to the requested mode. The line and
// end_line of the hit are left alone in every mode: they say where the match
// is, which stays true however much of it is quoted back.
func renderSnippet(body, mode string) string {
	switch mode {
	case SnippetNone:
		return ""
	case SnippetLine:
		if i := strings.IndexByte(body, '\n'); i >= 0 {
			body = body[:i]
		}
		return strings.TrimRight(body, "\r")
	default:
		return body
	}
}

// toDiagnostics reads the search pipeline's own account of the run out of the
// metadata map it reports it in.
//
// The map is what the searchers and the rerank stage write as they go, so a key
// is absent whenever the stage that writes it did not run — an unset
// `degraded`, for instance, means every configured searcher answered, not that
// nobody looked. Handing that map to a client would make its absences the
// client's problem; a typed struct turns them into definite booleans, which is
// the whole point of the field being opt-in rather than raw.
func toDiagnostics(meta map[string]interface{}) *SearchDiagnostics {
	out := &SearchDiagnostics{}
	if meta == nil {
		return out
	}
	out.Degraded, _ = meta["degraded"].(bool)
	out.Searchers, _ = meta["searchers"].([]string)
	out.FailedSearchers, _ = meta["failed_searchers"].([]string)
	out.SearcherErrors, _ = meta["searcher_errors"].(map[string]string)
	out.Reranked, _ = meta["reranked"].(bool)
	out.RerankCandidates, _ = meta["rerank_candidates"].(int)
	out.RerankError, _ = meta["rerank_error"].(string)
	return out
}

func toRepo(r *repos.Repo) *Repo {
	if r == nil {
		return nil
	}
	return &Repo{
		ID: r.ID, Name: r.Name, Source: string(r.Source), URL: r.URL,
		Path: r.Path, Branch: r.Branch, IndexedAt: r.IndexedAt,
		Status: string(r.Status), CreatedAt: r.CreatedAt, LastError: r.LastError,
		LastCommit: r.LastCommit, PendingCommit: r.PendingCommit,
		Active: r.Active,
	}
}

// toRepos keeps a nil list nil, which serializes as `null`. An empty list
// would serialize as `[]`, and the two are not the same answer to a client
// that distinguishes them.
func toRepos(in []*repos.Repo) []*Repo {
	if in == nil {
		return nil
	}
	out := make([]*Repo, 0, len(in))
	for _, r := range in {
		out = append(out, toRepo(r))
	}
	return out
}

func toIndexAck(a *service.IndexAck) *IndexAck {
	if a == nil {
		return nil
	}
	return &IndexAck{
		Status: a.Status, Queued: a.Queued, JobID: a.JobID, JobStatus: a.JobStatus,
		Force: a.Force, QueuedAt: a.QueuedAt, ClaimedBy: a.ClaimedBy, RepoStatus: a.RepoStatus,
	}
}

func toCoverageKind(k service.CoverageKind) CoverageKind {
	return CoverageKind{Kind: k.Kind, Candidates: k.Candidates, Edges: k.Edges, Ratio: k.Ratio}
}

func toCoverage(r *service.CoverageReport) *Coverage {
	if r == nil {
		return nil
	}
	out := &Coverage{
		RepoID: r.RepoID, Reported: r.Reported,
		UpdatedAt: r.UpdatedAt, IndexedAt: r.IndexedAt,
		Kinds:  make([]CoverageKind, 0, len(r.Kinds)),
		Totals: toCoverageKind(r.Totals),
	}
	for _, k := range r.Kinds {
		out.Kinds = append(out.Kinds, toCoverageKind(k))
	}
	return out
}

// toServices narrows the service graph to the named repositories and caps each
// list at limit (0 caps nothing).
//
// A link survives the filter when *either* end is in the selection: the reason
// to ask about one repository's services is usually what they talk to, and the
// far side of a cross-service call lives in another repository by definition.
func toServices(services []*graph.ServiceInfo, links []*graph.ServiceLink, repoIDs []string, limit int) *ServicesResponse {
	out := &ServicesResponse{Services: []*ServiceInfo{}, Links: []*ServiceLink{}}
	want := make(map[string]bool, len(repoIDs))
	for _, id := range repoIDs {
		want[id] = true
	}
	for _, s := range services {
		if len(want) == 0 || want[s.RepoID] {
			out.Services = append(out.Services, &ServiceInfo{
				RepoID: s.RepoID, Name: s.Name, Root: s.Root,
				DetectedBy: s.DetectedBy, UnitID: s.UnitID,
			})
		}
	}
	for _, l := range links {
		if len(want) == 0 || want[l.SrcRepo] || want[l.DstRepo] {
			out.Links = append(out.Links, &ServiceLink{
				SrcRepo: l.SrcRepo, SrcService: l.SrcService,
				DstRepo: l.DstRepo, DstService: l.DstService,
				Kind: l.Kind, Via: l.Via, Count: l.Count, Confidence: l.Confidence,
			})
		}
	}
	if limit > 0 {
		if len(out.Services) > limit {
			out.Services, out.Truncated = out.Services[:limit], true
		}
		if len(out.Links) > limit {
			out.Links, out.Truncated = out.Links[:limit], true
		}
	}
	return out
}

func toTopicNodes(in []*graph.Node) []*TopicNode {
	out := make([]*TopicNode, 0, len(in))
	for _, n := range in {
		if n == nil {
			continue
		}
		out = append(out, &TopicNode{Unit: toUnit(n.Unit), Service: n.Service})
	}
	return out
}

func toTopics(in []*graph.TopicInfo) *TopicsResponse {
	out := &TopicsResponse{Topics: make([]*TopicInfo, 0, len(in))}
	for _, t := range in {
		if t == nil {
			continue
		}
		out.Topics = append(out.Topics, &TopicInfo{
			Topic:       t.Topic,
			Producers:   toTopicNodes(t.Producers),
			Consumers:   toTopicNodes(t.Consumers),
			Description: t.Description,
			Declared:    t.Declared,
		})
	}
	return out
}

// --- response size budget ---

// fitSearch trims the response to maxBytes, dropping the lowest-ranked hits,
// and records that it did.
func fitSearch(resp *SearchResponse, maxBytes int) {
	all := resp.Hits
	keep := fitToBudget(len(all), maxBytes, func(n int) int {
		probe := *resp
		probe.Hits, probe.Truncated = all[:n], n < len(all)
		return encodedLen(probe)
	})
	resp.Hits, resp.Truncated = all[:keep], keep < len(all)
}

// fitContext trims the package to maxBytes, dropping the lowest-ranked items,
// and records that it did. A nil response is what toContext answers a nil
// result with; there is nothing in it to trim.
func fitContext(resp *ContextResponse, maxBytes int) {
	if resp == nil {
		return
	}
	all := resp.Items
	keep := fitToBudget(len(all), maxBytes, func(n int) int {
		probe := *resp
		probe.Items, probe.Truncated = all[:n], n < len(all)
		return encodedLen(probe)
	})
	resp.Items, resp.Truncated = all[:keep], keep < len(all)
}

// fitToBudget returns how many of n ranked elements fit in maxBytes, given a
// function that encodes the whole response built from a prefix of them.
//
// Dropping whole elements from the end is the only cut worth making: they
// arrive best-first, so the caller keeps the answers it most wanted, and
// truncating the encoded bytes instead would hand back JSON that does not
// parse. `encoded` is asked about a prefix *with the truncation flag already
// set*, because that flag is part of what has to fit — a response that promises
// a budget and then exceeds it by the width of its own warning is not honest
// about either. Encoded size grows with the prefix length, so a binary search
// finds the longest one in a handful of encodings rather than n of them.
func fitToBudget(n, maxBytes int, encoded func(keep int) int) int {
	if maxBytes <= 0 || n == 0 || encoded(n) <= maxBytes {
		return n
	}
	lo, hi := 0, n-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if encoded(mid) <= maxBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// encodedLen is the size of the body writeJSON would send for v, including the
// newline json.Encoder terminates it with.
func encodedLen(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b) + 1
}

func toUnit(u *storage.ASTUnit) *Unit {
	if u == nil {
		return nil
	}
	return &Unit{
		ID: u.ID, RepoID: u.RepoID, FilePath: u.FilePath, Language: u.Language,
		Kind: u.Kind, Name: u.Name, Qualified: u.Qualified, ParentID: u.ParentID,
		StartLine: u.StartLine, EndLine: u.EndLine,
		Signature: u.Signature, Doc: u.Doc,
		Summary: storage.DecodeUnitMeta(u.Meta).Summary,
	}
}

func toGraphEdge(e *storage.Edge) *GraphEdge {
	if e == nil {
		return nil
	}
	m := storage.DecodeEdgeMeta(e.Meta)
	return &GraphEdge{
		ID: e.ID, RepoID: e.RepoID, SrcID: e.SrcID, DstID: e.DstID,
		DstRepoID: e.DstRepoID, Kind: e.Kind, DstName: e.DstName,
		FilePath: e.FilePath, Line: e.Line, Confidence: e.Confidence,
		Topic: m.Topic, Path: m.Path, Method: m.Method,
	}
}

func toJob(j *storage.IndexJob) *Job {
	if j == nil {
		return nil
	}
	return &Job{
		ID: j.ID, RepoID: j.RepoID, Kind: j.Kind, Force: j.Force,
		Status: j.Status, Error: j.Error, CreatedAt: j.CreatedAt,
		ClaimedAt: j.ClaimedAt, HeartbeatAt: j.HeartbeatAt, ClaimedBy: j.ClaimedBy,
	}
}

func toJobs(in []*storage.IndexJob) []*Job {
	out := make([]*Job, 0, len(in))
	for _, j := range in {
		out = append(out, toJob(j))
	}
	return out
}

func toNeighbors(res *graph.NeighborsResult) *NeighborsResponse {
	if res == nil {
		return nil
	}
	out := &NeighborsResponse{Out: []*EdgeHop{}, In: []*EdgeHop{}}
	if res.Center != nil {
		out.Center = toUnit(res.Center.Unit)
		if out.Center != nil {
			out.Center.Service = res.Center.Service
		}
	}
	for _, e := range res.Out {
		out.Out = append(out.Out, &EdgeHop{Edge: toGraphEdge(e.Edge), Unit: toUnit(e.Unit)})
	}
	for _, e := range res.In {
		out.In = append(out.In, &EdgeHop{Edge: toGraphEdge(e.Edge), Unit: toUnit(e.Unit)})
	}
	return out
}

func toPath(steps []*graph.PathStep) []*PathStep {
	out := make([]*PathStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, &PathStep{Edge: toGraphEdge(s.Edge), Unit: toUnit(s.Unit), Via: s.Via})
	}
	return out
}

func toTraceSteps(steps []*graph.TraceStep) []*TraceStep {
	out := make([]*TraceStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, &TraceStep{
			Unit: toUnit(s.Unit), Service: s.Service, Tracked: s.Tracked,
			Via: s.Via, Note: s.Note, Line: s.Line, Confidence: s.Confidence,
		})
	}
	return out
}

func toTrace(res *graph.TraceResult) *TraceResponse {
	if res == nil {
		return nil
	}
	out := &TraceResponse{Param: res.Param, Chains: res.Chains, Steps: toTraceSteps(res.Steps)}
	for _, alt := range res.Alternatives {
		out.Alternatives = append(out.Alternatives, toTraceSteps(alt))
	}
	return out
}

func toContext(res *service.ContextResult, snippet string) *ContextResponse {
	if res == nil {
		return nil
	}
	out := &ContextResponse{
		Query: res.Query, Mode: res.Mode, RewrittenQuery: res.RewrittenQuery,
		Items: make([]*ContextItem, 0, len(res.Items)),
	}
	for _, item := range res.Items {
		dto := &ContextItem{Hit: toSearchHit(item.Hit, snippet), Service: item.Service, Unit: toUnit(item.Unit)}
		for _, rel := range item.Related {
			dto.Related = append(dto.Related, &RelatedUnit{
				Unit: toUnit(rel.Unit), Service: rel.Service,
				Via: rel.Via, Direction: rel.Direction, Distance: rel.Distance,
			})
		}
		out.Items = append(out.Items, dto)
	}
	return out
}
