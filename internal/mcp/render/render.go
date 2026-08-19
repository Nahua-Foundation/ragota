// Package render turns ragota's answers into the text a tool result carries.
//
// The API already speaks JSON, and returning it verbatim would be less code.
// It is not what this package does, because every byte of a tool result lands in
// the context window of the model that asked: a search hit costs about 150 bytes
// as a JSON object and about 60 as a line, and the line is the one a model reads
// without effort. The renderers below are therefore terse by design — one item
// per line, ids kept because other tools take them, and nothing repeated that
// the header already said.
//
// Three things are never omitted to save room: that a budget dropped results,
// that retrieval ran degraded, and that an empty answer is empty. A trimmed
// answer with no account of itself is exactly the failure the budget was
// supposed to make safe.
package render

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/client"
)

// relatedPerItem bounds how many graph neighbours one /context item renders.
// The server already caps them at 24 per item; at five items that is 120 lines
// of neighbours around five hits, which is the expansion drowning the thing it
// was meant to explain.
const relatedPerItem = 6

// edgesPerDirection bounds each side of a neighbours answer. /graph/neighbors
// caps neither list on purpose, so a unit that everything calls comes back with
// every edge it has, and something has to stop it here.
const edgesPerDirection = 25

// Search renders ranked hits, and says why an empty list is empty.
func Search(res *client.SearchResponse) string {
	var b builder

	switch {
	case len(res.Hits) == 0:
		b.line("No hits for %q (mode %s).", res.Query, res.Mode)
	case res.Truncated:
		b.line("%d hits for %q (mode %s), %d shown within the byte budget.",
			res.Total, res.Query, res.Mode, len(res.Hits))
	default:
		b.line("%d hits for %q (mode %s).", len(res.Hits), res.Query, res.Mode)
	}

	for i, h := range res.Hits {
		b.line("%d. %s%s%s", i+1, location(h.RepoID, h.FilePath, h.Line, h.EndLine),
			symbolOf(h.Symbol, h.Kind, h.Language), bracket(h.Reason))
		b.indented(h.Snippet)
	}

	b.blank()
	b.write(Diagnostics(res.Diagnostics, len(res.Hits) == 0))
	if res.Truncated {
		b.line("Budget note: max_bytes dropped %d of the %d hits retrieved. Raise max_bytes, or lower limit, to see them.",
			res.Total-len(res.Hits), res.Total)
	}
	return b.String()
}

// Context renders search hits with the graph expanded around each one.
func Context(res *client.ContextResponse) string {
	var b builder

	if len(res.Items) == 0 {
		b.line("No items for %q (mode %s).", res.Query, res.Mode)
	} else {
		b.line("%d items for %q (mode %s).", len(res.Items), res.Query, res.Mode)
	}
	if res.RewrittenQuery != "" && res.RewrittenQuery != res.Query {
		b.line("Searched as: %q.", res.RewrittenQuery)
	}

	for i, it := range res.Items {
		h := it.Hit
		if h == nil {
			continue
		}
		b.line("%d. %s%s%s", i+1, location(h.RepoID, h.FilePath, h.Line, h.EndLine),
			symbolOf(h.Symbol, h.Kind, h.Language), bracket(h.Reason))
		if it.Unit != nil {
			b.line("   unit %s%s%s", it.Unit.ID, field(" service", it.Service), field(" ", it.Unit.Signature))
			b.line("   %s", strings.TrimSpace(it.Unit.Summary))
		}
		b.indented(h.Snippet)

		shown := it.Related
		if len(shown) > relatedPerItem {
			shown = shown[:relatedPerItem]
		}
		for _, r := range shown {
			if r.Unit == nil {
				continue
			}
			b.line("   %s d%d %s %s%s%s", arrow(r.Direction), r.Distance, r.Via,
				location(r.Unit.RepoID, r.Unit.FilePath, r.Unit.StartLine, r.Unit.EndLine),
				symbolOf(r.Unit.Name, r.Unit.Kind, ""), field(" service", r.Service))
		}
		if n := len(it.Related) - len(shown); n > 0 {
			b.line("   ... %d more related units; ragota_neighbors on unit %s has all of them.", n, unitID(it.Unit))
		}
	}

	b.blank()
	if res.Truncated {
		b.line("Budget note: max_bytes dropped whole items that were retrieved. Raise max_bytes, or lower limit and hops, to see them.")
	}
	return b.String()
}

// Symbols renders identifier lookups. term is what was asked for, so that an
// empty answer names the thing that was not found.
func Symbols(res *client.SymbolResponse, term string) string {
	var b builder
	if len(res.Symbols) == 0 {
		b.line("No symbol matches %s.", quoteOr(term, "the given filters"))
		b.blank()
		b.line("Matching is on the name, case-insensitively, not on meaning. A question phrased in prose belongs in ragota_search.")
		return b.String()
	}

	b.line("%d symbols matching %s (exact matches first).", len(res.Symbols), quoteOr(term, "the given filters"))
	for i, s := range res.Symbols {
		b.line("%d. %s%s%s", i+1, location(s.RepoID, s.FilePath, s.StartLine, s.EndLine),
			symbolOf(s.Name, s.Kind, s.Language), field(" ", s.Qualified))
		b.indented(firstLine(s.Doc))
	}
	return b.String()
}

// References renders the sites that reference a symbol.
func References(res *client.ReferencesResponse) string {
	var b builder
	if len(res.References) == 0 {
		b.line("No references. Nothing is defined at that position, or nothing in the index points at it.")
		b.blank()
		b.line("Lines are 1-based, and line 0 always answers empty. Check ragota_status if the repository may not be indexed.")
		return b.String()
	}

	b.line("%d references.", len(res.References))
	for _, r := range res.References {
		// `target` empty marks an edge that names the symbol without ever having
		// been resolved to it — a lead, not a fact, and the caller should know
		// which kind it is holding.
		suffix := " (unresolved)"
		if r.Target != "" {
			suffix = " -> " + r.Target
		}
		b.line("   %s %s %s%s", location(r.RepoID, r.FilePath, r.Line, 0), r.Kind, r.Word, suffix)
	}
	return b.String()
}

// Neighbors renders the edges around one unit.
func Neighbors(res *client.NeighborsResponse) string {
	var b builder
	if res.Center == nil {
		return "No such unit. Unit ids come from another answer and do not survive a reindex; fetch a fresh one."
	}

	c := res.Center
	b.line("center %s%s unit %s", location(c.RepoID, c.FilePath, c.StartLine, c.EndLine),
		symbolOf(c.Name, c.Kind, c.Language), c.ID)
	if c.Signature != "" {
		b.line("   %s", c.Signature)
	}

	b.blank()
	renderHops(&b, "out", "->", res.Out)
	b.blank()
	renderHops(&b, "in", "<-", res.In)
	return b.String()
}

func renderHops(b *builder, label, mark string, hops []*client.EdgeHop) {
	shown := hops
	if len(shown) > edgesPerDirection {
		shown = shown[:edgesPerDirection]
	}
	if len(hops) == 0 {
		b.line("%s: none.", label)
		return
	}
	b.line("%s (%d):", label, len(hops))
	for _, h := range shown {
		e := h.Edge
		if e == nil {
			continue
		}
		if h.Unit != nil {
			b.line("   %s %s %.2f %s%s unit %s", mark, e.Kind, e.Confidence,
				location(h.Unit.RepoID, h.Unit.FilePath, h.Unit.StartLine, h.Unit.EndLine),
				symbolOf(h.Unit.Name, h.Unit.Kind, ""), h.Unit.ID)
			continue
		}
		// No far-side unit is the ordinary shape of a call into something not
		// indexed here — a third-party library, or a service nobody registered.
		b.line("   %s %s %.2f %s (not indexed here) from %s", mark, e.Kind, e.Confidence,
			quoteOr(contractOf(e), "unnamed"), location(e.RepoID, e.FilePath, e.Line, 0))
	}
	if n := len(hops) - len(shown); n > 0 {
		b.line("   ... %d more %s edges not shown.", n, label)
	}
}

// Path renders a directed path between two units.
func Path(res *client.GraphPathResponse) string {
	var b builder
	if len(res.Steps) == 0 {
		return "No path. Either nothing reaches the destination within max_depth, or from_unit_id names no unit at all — the API does not distinguish the two."
	}

	b.line("Path of %d steps.", res.Length)
	for i, s := range res.Steps {
		if s.Unit == nil {
			continue
		}
		b.line("%d.%s %s%s unit %s", i+1, field(" via", s.Via),
			location(s.Unit.RepoID, s.Unit.FilePath, s.Unit.StartLine, s.Unit.EndLine),
			symbolOf(s.Unit.Name, s.Unit.Kind, ""), s.Unit.ID)
	}
	return b.String()
}

// Trace renders where a parameter flows. alternatives asks for the runner-up
// chains, which are off by default because each one costs as much as the best.
func Trace(res *client.TraceResponse, symbol string, alternatives bool) string {
	var b builder
	if len(res.Steps) == 0 {
		return fmt.Sprintf("No flow found for %q out of %q. The parameter name is matched on word boundaries ignoring case and underscores, so \"user_id\" follows userId but \"user\" follows neither username nor user_agent.", res.Param, symbol)
	}

	b.line("%q out of %s: %d chains found, best one below.", res.Param, symbol, res.Chains)
	renderChain(&b, res.Steps)

	if alternatives {
		for i, alt := range res.Alternatives {
			b.blank()
			b.line("alternative %d:", i+1)
			renderChain(&b, alt)
		}
	} else if n := len(res.Alternatives); n > 0 {
		b.blank()
		b.line("%d alternative chains withheld; set include_alternatives to see them.", n)
	}

	b.blank()
	b.line("Confidence is cumulative along the chain, so a long chain is a lead to verify, not a fact.")
	return b.String()
}

func renderChain(b *builder, steps []*client.TraceStep) {
	for i, s := range steps {
		if s.Unit == nil {
			continue
		}
		b.line("%d.%s %s%s conf %.2f tracked %s", i+1, field(" via", s.Via),
			location(s.Unit.RepoID, s.Unit.FilePath, lineOr(s.Line, s.Unit.StartLine), 0),
			symbolOf(s.Unit.Name, s.Unit.Kind, ""), s.Confidence, strings.Join(s.Tracked, ","))
		if s.Note != "" {
			b.line("   %s", s.Note)
		}
	}
}

// Services renders the service map.
func Services(res *client.ServicesResponse) string {
	var b builder
	if len(res.Services) == 0 {
		return "No services detected. Nothing is indexed, or no repository declares one — see ragota_progress."
	}

	b.line("%d services, %d links.", len(res.Services), len(res.Links))
	b.blank()
	b.line("services:")
	for _, s := range res.Services {
		b.line("   %s/%s root %s (%s) unit %s", s.RepoID, s.Name, pathOr(s.Root), s.DetectedBy, s.UnitID)
	}

	b.blank()
	if len(res.Links) == 0 {
		b.line("links: none. No cross-service call was resolved; ragota_status reports contract coverage, which says whether that is the estate or the indexer.")
	} else {
		b.line("links:")
		for _, l := range res.Links {
			b.line("   %s/%s -> %s/%s %s %s x%d conf %.2f",
				l.SrcRepo, l.SrcService, l.DstRepo, l.DstService, l.Kind, l.Via, l.Count, l.Confidence)
		}
	}
	if res.Truncated {
		b.blank()
		b.line("Budget note: limit cut one of the two lists short. Raise limit, or narrow with repos.")
	}
	return b.String()
}

// Topics renders messaging topics with the code on both ends.
func Topics(res *client.TopicsResponse) string {
	var b builder
	if len(res.Topics) == 0 {
		return "No topics. Nothing indexed produces or consumes one, and no AsyncAPI channel declares one."
	}

	b.line("%d topics.", len(res.Topics))
	for _, t := range res.Topics {
		b.blank()
		b.line("%s%s%s", t.Topic, declared(t.Declared), field(" -", firstLine(t.Description)))
		renderTopicNodes(&b, "produced by", t.Producers)
		renderTopicNodes(&b, "consumed by", t.Consumers)
	}
	return b.String()
}

func renderTopicNodes(b *builder, label string, nodes []*client.TopicNode) {
	if len(nodes) == 0 {
		b.line("   %s: nothing indexed here.", label)
		return
	}
	for _, n := range nodes {
		if n.Unit == nil {
			continue
		}
		b.line("   %s: %s%s%s", label,
			location(n.Unit.RepoID, n.Unit.FilePath, n.Unit.StartLine, n.Unit.EndLine),
			symbolOf(n.Unit.Name, n.Unit.Kind, ""), field(" service", n.Service))
	}
}

// Status renders what is indexed and how far it can be trusted. repos, stats and
// coverage may each be nil where the caller did not ask for them or the server
// did not answer.
func Status(health *client.HealthResponse, baseURL string, repos []*client.Repo, stats *client.StatsResponse, cov *client.Coverage) string {
	var b builder

	if health != nil {
		b.line("ragota at %s: %s, api %s, build %s.", baseURL, health.Status, health.APIVersion, health.Version)
	} else {
		b.line("ragota at %s.", baseURL)
	}

	if stats != nil && len(stats.Indexers) > 0 {
		b.blank()
		b.line("indexes:")
		for _, name := range sortedKeys(stats.Indexers) {
			s := stats.Indexers[name]
			b.line("   %s %d documents%s%s", name, s.Documents, repoCount(s.Repos), size(s.SizeBytes))
		}
	}

	b.blank()
	if len(repos) == 0 {
		b.line("repositories: none registered. Every retrieval answer will be empty until an operator adds and indexes one; this server cannot, by design.")
	} else {
		// Active and dormant is the difference between "this code is not
		// indexed" and "this code is indexed but the deployment is not looking
		// at it right now", and only one of those is the caller's problem.
		// Listing the names without the distinction is how a reader concludes
		// it may ask about all of them, and then reads an empty answer as
		// absence.
		active := 0
		for _, r := range repos {
			if r.Active {
				active++
			}
		}
		b.line("repositories (%d, %d in the working set):", len(repos), active)
		for _, r := range repos {
			state := "dormant"
			if r.Active {
				state = "active "
			}
			b.line("   %s %s name %s %s indexed %s%s", state, r.ID, r.Name, r.Status, stamp(r.IndexedAt), field(" error", firstLine(r.LastError)))
		}
		if active < len(repos) {
			b.line("   Retrieval without a repo argument answers from the working set only. Naming a dormant repository in `repo` reaches it; the graph tools reach it either way.")
		}
	}

	if cov != nil {
		b.blank()
		b.write(coverage(cov))
	}
	return b.String()
}

// coverage renders one repository's contract coverage, which is the answer to
// "is an empty graph result the estate or the indexer".
func coverage(c *client.Coverage) string {
	var b builder
	if !c.Reported {
		b.line("coverage for %s: never reported. No coverage-writing index pass has run, so the counters would be meaningless rather than zero.", c.RepoID)
		return b.String()
	}

	b.line("coverage for %s (summary %s, index %s):", c.RepoID, stamp(c.UpdatedAt), stamp(c.IndexedAt))
	b.line("   all %d/%d %.2f", c.Totals.Edges, c.Totals.Candidates, c.Totals.Ratio)
	for _, k := range c.Kinds {
		if k.Candidates == 0 {
			continue
		}
		b.line("   %s %d/%d %.2f", k.Kind, k.Edges, k.Candidates, k.Ratio)
	}
	if c.Totals.Ratio < 0.8 {
		b.line("A ratio well below 1 means the indexer did not understand many of this repository's outbound calls, so an empty graph answer here is not evidence that the call does not exist.")
	}
	if c.UpdatedAt != 0 && c.IndexedAt > c.UpdatedAt {
		b.line("The summary predates the current index and describes an earlier pass.")
	}
	return b.String()
}

// Diagnostics turns the search diagnostics into the one or two sentences that
// decide how a caller should read a thin answer.
//
// It says nothing at all on the ordinary path — a healthy answer with hits needs
// no commentary, and commentary is bytes. It speaks when retrieval was degraded,
// and when the answer was empty, because "empty" and "empty from a broken
// backend" are the two results a caller must never confuse.
func Diagnostics(d *client.SearchDiagnostics, empty bool) string {
	if d == nil {
		if empty {
			return "Retrieval health was not requested for this call, so an empty answer cannot be told apart from a backend that was down.\n"
		}
		return ""
	}

	var b builder
	if d.Degraded {
		b.line("DEGRADED: %s did not answer (%s). These hits came from fewer indexes than this deployment has, so a thin or empty answer here is not evidence that the code is absent. Say so rather than concluding the code does not exist.",
			join(d.FailedSearchers, "a configured index"), reasons(d.SearcherErrors))
		return b.String()
	}
	if empty {
		b.line("Retrieval was not degraded, so this is the corpus's answer to this query, not a backend failure. If a match was expected: check ragota_status for whether the repository is indexed, send a bare identifier to ragota_symbol instead, or rephrase.")
	}
	return b.String()
}

// --- small helpers ---

// builder accumulates lines, dropping the empty ones a nil field would produce
// and collapsing runs of blanks, so that every renderer can append
// unconditionally without checking whether it has anything to say.
type builder struct {
	sb strings.Builder
}

func (b *builder) line(format string, args ...any) {
	s := strings.TrimRight(fmt.Sprintf(format, args...), " ")
	if strings.TrimSpace(s) == "" {
		return
	}
	b.sb.WriteString(s)
	b.sb.WriteByte('\n')
}

func (b *builder) write(s string) {
	if s == "" {
		return
	}
	b.sb.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		b.sb.WriteByte('\n')
	}
}

func (b *builder) blank() {
	s := b.sb.String()
	if s == "" || strings.HasSuffix(s, "\n\n") {
		return
	}
	b.sb.WriteByte('\n')
}

// indented writes a code body under the line it belongs to. Three spaces are
// enough to separate it from the hit list and shift every line equally, so a
// caller pasting Python back out still has valid indentation.
func (b *builder) indented(code string) {
	if strings.TrimSpace(code) == "" {
		return
	}
	for _, l := range strings.Split(strings.TrimRight(code, "\n"), "\n") {
		b.sb.WriteString("   ")
		b.sb.WriteString(l)
		b.sb.WriteByte('\n')
	}
}

// String returns the accumulated text with exactly one trailing newline, and
// nothing at all when nothing was written: a renderer with no comment to make
// must not contribute a blank line to the answer it is appended to.
func (b *builder) String() string {
	s := strings.TrimRight(b.sb.String(), "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}

// location renders repo, path and line range as one token an agent can act on.
func location(repoID, path string, line, endLine int) string {
	switch {
	case line <= 0:
		return fmt.Sprintf("%s %s", repoID, path)
	case endLine > line:
		return fmt.Sprintf("%s %s:%d-%d", repoID, path, line, endLine)
	default:
		return fmt.Sprintf("%s %s:%d", repoID, path, line)
	}
}

func symbolOf(name, kind, language string) string {
	if name == "" && kind == "" {
		return ""
	}
	attrs := strings.Join(nonEmpty(kind, language), ", ")
	if attrs == "" {
		return " " + name
	}
	if name == "" {
		return fmt.Sprintf(" (%s)", attrs)
	}
	return fmt.Sprintf(" %s (%s)", name, attrs)
}

func bracket(s string) string {
	if s == "" {
		return ""
	}
	return " [" + s + "]"
}

func field(label, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return label + " " + strings.TrimSpace(value)
}

func arrow(direction string) string {
	if direction == "in" {
		return "<-"
	}
	return "->"
}

// contractOf names what an unresolved edge points at: the contract it declares,
// or the callee it merely names.
func contractOf(e *client.GraphEdge) string {
	switch {
	case e.Topic != "":
		return "topic:" + e.Topic
	case e.Path != "":
		return strings.TrimSpace(e.Method + " " + e.Path)
	default:
		return e.DstName
	}
}

func declared(b bool) string {
	if b {
		return " (declared)"
	}
	return ""
}

func quoteOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return fmt.Sprintf("%q", s)
}

func pathOr(s string) string {
	if s == "" {
		return "."
	}
	return s
}

func lineOr(line, fallback int) int {
	if line > 0 {
		return line
	}
	return fallback
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func unitID(u *client.Unit) string {
	if u == nil {
		return "?"
	}
	return u.ID
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func join(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return strings.Join(values, ", ")
}

func reasons(m map[string]string) string {
	if len(m) == 0 {
		return "no reason reported"
	}
	parts := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		parts = append(parts, k+": "+firstLine(m[k]))
	}
	return strings.Join(parts, "; ")
}

// repoCount is omitted at zero rather than printed: only the vector indexer
// fills it, and "0 repos" beside a million documents reads as a fault.
func repoCount(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(", %d repos", n)
}

func size(bytes int64) string {
	if bytes <= 0 {
		return ""
	}
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf(", %d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf(", %.1f %cB", float64(bytes)/float64(div), "KMGT"[exp])
}

// stamp renders a unix second as UTC. 0 means the event never happened, which is
// a different thing from 1970 and has to read as one.
func stamp(unix int64) string {
	if unix <= 0 {
		return "never"
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// sortedKeys keeps map-backed output stable, so that two identical calls render
// identically and a test can assert on the text.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
