package enrich

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/index"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// A question is asked in the language of the domain; code is named in the
// language of its implementation. "Which allocation decider stops shards from
// being allocated to nodes low on disk space" shares no token with
// DiskThresholdDecider.canAllocate, so neither BM25 nor an embedder can bridge
// it — measured, that gap is most of what the retrieval evaluation still
// misses.
//
// It cannot be closed on the query side: rewriting the question into keywords
// was measured and lost, because the model drops the literal tokens that do
// match and destroys the phrasing intent detection reads. So it is closed on
// the index side instead — one line per symbol saying what it does, written
// once at index time and embedded with the symbol, where it costs nothing per
// request and works for every language the same way.
//
// Which symbols: the ones the graph already marks as service boundaries — the
// endpoints of HTTP, RPC, messaging and table contracts — and only those with
// no doc comment of their own. On the evaluation corpus that is 2 118 symbols
// across nine repositories against 146 000 total, which is the difference
// between a pass that runs in minutes and one nobody will ever enable.
//
// MEASURED, AND IT DOES NOT PAY. Off by default; do not enable without
// measuring on your own corpus. Two runs with qwen2.5:1.5b, 1 628 summaries
// over nine repositories and 1 217 over the three where the vocabulary gap
// actually lives: every retrieval metric unchanged to three decimals except
// MRR, which moved -0.011 and -0.002. The summaries themselves are fine
// ("marks indices as frozen based on cluster state metadata"); they simply
// change no ranking.
//
// The reason is worth keeping, because it also explains why query rewriting
// lost: the rerank stage is already the vocabulary bridge, and it is a better
// one. A cross-encoder reads the question and the card together at query
// time, so it bridges the gap knowing what was actually asked; a summary
// written at index time has to guess which question will be asked, and a
// guess embedded into a card competes with the code around it. Index-time
// enrichment is a worse instrument for the same job — and by the time the
// reranker had landed, the questions this pass targeted were already being
// answered.

const (
	// symbolBatchSize is how many symbols go into one LLM request, and
	// symbolBodyLines how much of each body the model is shown. Batching is
	// what makes the pass affordable — per-symbol calls would be 10x the round
	// trips for the same tokens — but the product of the two has to stay
	// inside the serving context of a small local model, which is where this
	// pass is meant to run.
	symbolBatchSize = 10
	// defaultMaxSymbolSummaries caps summarized symbols per repo per run.
	defaultMaxSymbolSummaries = 500
	symbolBodyLines           = 12
	// maxSummaryChars bounds one returned line; a model that ignores "one
	// sentence" must not put a page into the index.
	maxSummaryChars = 240
)

// boundaryEdgeKinds are the edges whose endpoints sit on a service boundary:
// the code that serves, calls, publishes, consumes or persists a contract.
var boundaryEdgeKinds = []string{
	store.EdgeHandledBy, store.EdgeImplementsRPC,
	store.EdgeHTTPCall, store.EdgeRPCCall,
	store.EdgeProduces, store.EdgeConsumes,
	store.EdgeWritesTo, store.EdgeReadsFrom,
}

// summarizableKinds are the unit kinds worth a sentence. A field or a constant
// is named by its type and its owner; a function is not.
var summarizableKinds = map[string]bool{
	"function": true, "method": true, "class": true, "interface": true,
}

// SetSymbolSummaries enables one-line summaries of boundary symbols.
// max <= 0 selects the default cap.
func (e *Enricher) SetSymbolSummaries(enabled bool, max int) {
	e.symSummaries = enabled
	if max <= 0 {
		max = defaultMaxSymbolSummaries
	}
	e.maxSymSumm = max
}

// SummarizeSymbols writes one line per boundary symbol and re-indexes the
// files that own them, so the lines reach the vector index attached to their
// symbol. It is best-effort enrichment: every failure is logged and the index
// stands without it.
func (e *Enricher) SummarizeSymbols(ctx context.Context, repo *domain.Repo) error {
	if !e.symSummaries || e.generator == nil {
		return nil
	}
	units, err := e.boundarySymbols(ctx, repo.ID)
	if err != nil || len(units) == 0 {
		return err
	}

	written := 0
	byFile := map[string][]*domain.ASTUnit{}
	for start := 0; start < len(units); start += symbolBatchSize {
		if ctx.Err() != nil {
			break
		}
		batch := units[start:min(start+symbolBatchSize, len(units))]
		summaries, gerr := e.summarizeBatch(ctx, repo, batch)
		if gerr != nil {
			slog.Warn("symbol summary batch failed", "repo_id", repo.ID, "err", gerr)
			continue
		}
		for i, text := range summaries {
			if text == "" {
				continue
			}
			u := batch[i]
			meta := store.DecodeUnitMeta(u.Meta)
			meta.Summary = text
			u.Meta = store.EncodeUnitMeta(meta)
			if serr := e.store.StoreASTUnit(ctx, u); serr != nil {
				slog.Warn("store symbol summary", "unit", u.Qualified, "err", serr)
				continue
			}
			byFile[u.FilePath] = append(byFile[u.FilePath], u)
			written++
		}
	}

	slog.Info("symbol summaries", "repo_id", repo.ID, "written", written, "candidates", len(units))
	if written == 0 {
		return nil
	}
	return e.reindexAnnotated(ctx, repo, byFile)
}

// boundarySymbols returns the undocumented symbols that sit on a contract
// boundary, in a stable order, capped.
func (e *Enricher) boundarySymbols(ctx context.Context, repoID string) ([]*domain.ASTUnit, error) {
	edges, err := e.store.GetEdges(ctx, domain.QueryOpts{RepoID: repoID, Kinds: boundaryEdgeKinds})
	if err != nil {
		return nil, fmt.Errorf("boundary edges: %w", err)
	}
	ids := make([]string, 0, len(edges)*2)
	for _, e := range edges {
		if e.SrcID != "" && e.SrcID != "0" {
			ids = append(ids, e.SrcID)
		}
		if e.DstID != "" && e.DstID != "0" {
			ids = append(ids, e.DstID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	units, err := e.store.GetASTUnitsByIDs(ctx, dedupStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("boundary units: %w", err)
	}

	out := units[:0]
	for _, u := range units {
		switch {
		case !summarizableKinds[u.Kind]:
		case strings.TrimSpace(u.Doc) != "": // it documents itself already
		case repos.IsTestPath(u.FilePath):
		case store.DecodeUnitMeta(u.Meta).Summary != "": // already summarized
		default:
			out = append(out, u)
		}
	}
	// Deterministic order, so a capped run summarizes the same symbols twice
	// and a re-run is idempotent rather than a lottery.
	sort.Slice(out, func(i, j int) bool {
		if out[i].FilePath != out[j].FilePath {
			return out[i].FilePath < out[j].FilePath
		}
		return out[i].StartLine < out[j].StartLine
	})
	if e.maxSymSumm > 0 && len(out) > e.maxSymSumm {
		out = out[:e.maxSymSumm]
	}
	return out, nil
}

// numberedLineRe matches "3. text" / "3) text" / "3 - text" in a model reply.
var numberedLineRe = regexp.MustCompile(`^\s*(\d{1,3})\s*[.):\-]\s*(.+)$`)

// summarizeBatch asks for one line per symbol and returns them positionally,
// with "" where the model gave nothing usable.
func (e *Enricher) summarizeBatch(ctx context.Context, repo *domain.Repo, batch []*domain.ASTUnit) ([]string, error) {
	var b strings.Builder
	b.WriteString("For each numbered code symbol below, write ONE sentence describing what it does, ")
	b.WriteString("in the words a developer would use to ask for it — the domain task, not the code. ")
	b.WriteString("Do not restate the symbol name. Reply with one numbered line per symbol and nothing else.\n\n")
	for i, u := range batch {
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, u.Kind, firstNonEmpty(u.Qualified, u.Name))
		if u.Signature != "" {
			b.WriteString("   " + oneLine(u.Signature) + "\n")
		}
		if body := e.symbolBody(repo, u); body != "" {
			b.WriteString(indentBlock(body))
		}
		b.WriteByte('\n')
	}

	out, err := e.generator.Generate(ctx, b.String())
	if err != nil {
		return nil, err
	}

	summaries := make([]string, len(batch))
	for _, line := range strings.Split(stripThinkBlocks(out), "\n") {
		m := numberedLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, cerr := strconv.Atoi(m[1])
		if cerr != nil || n < 1 || n > len(batch) {
			continue
		}
		summaries[n-1] = clampSummary(m[2])
	}
	return summaries, nil
}

// clampSummary trims a returned line to one sentence-ish of plain text.
func clampSummary(text string) string {
	text = oneLine(strings.Trim(strings.TrimSpace(text), "`*_\""))
	if len(text) > maxSummaryChars {
		cut := strings.LastIndexAny(text[:maxSummaryChars], " ")
		if cut < maxSummaryChars/2 {
			cut = maxSummaryChars
		}
		text = strings.TrimSpace(text[:cut])
	}
	return text
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func indentBlock(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		b.WriteString("   ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// symbolBody reads the first lines of a symbol from the working tree. A
// signature alone names the symbol again; the body is what says what it does.
func (e *Enricher) symbolBody(repo *domain.Repo, u *domain.ASTUnit) string {
	if repo.Path == "" || u.StartLine < 1 {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(repo.Path, filepath.FromSlash(u.FilePath)))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	start := u.StartLine - 1
	if start >= len(lines) {
		return ""
	}
	end := min(min(start+symbolBodyLines, u.EndLine), len(lines))
	if end <= start {
		end = min(start+1, len(lines))
	}
	return strings.Join(lines[start:end], "\n")
}

// reindexAnnotated re-runs the vector indexer over the files whose symbols
// were summarized, handing it the summaries as annotations. Cards are built
// from file content at index time, so this second pass is what carries the
// new sentences into the index; it touches only the affected files.
func (e *Enricher) reindexAnnotated(ctx context.Context, repo *domain.Repo, byFile map[string][]*domain.ASTUnit) error {
	vecIdx, ok := e.indexers[index.IndexTypeVector]
	if !ok || e.store.VectorStore() == nil {
		return nil // nothing to carry them into
	}

	files := make([]*index.FileToIndex, 0, len(byFile))
	annotations := make(map[string]string, len(byFile))
	for path, units := range byFile {
		data, err := os.ReadFile(filepath.Join(repo.Path, filepath.FromSlash(path)))
		if err != nil {
			continue
		}
		files = append(files, &index.FileToIndex{
			Path:     path,
			Language: units[0].Language,
			Content:  data,
		})
		for _, u := range units {
			meta := store.DecodeUnitMeta(u.Meta)
			annotations[index.AnnotationKey(path, u.StartLine)] = meta.Summary
		}
	}
	if len(files) == 0 {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	res, err := vecIdx.Index(ctx, &index.IndexRequest{
		RepoID:      repo.ID,
		RepoPath:    repo.Path,
		RepoName:    repo.Name,
		Files:       files,
		Annotations: annotations,
	})
	if err != nil {
		return fmt.Errorf("reindex annotated files: %w", err)
	}
	slog.Info("symbol summaries indexed", "repo_id", repo.ID,
		"files", len(files), "symbols", len(annotations), "failed", res.FilesFailed)
	return nil
}

// dedupStrings returns the non-empty values of in, in order, without repeats.
func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// firstNonEmpty returns the first value that is not empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
