package enrich

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/indexing"
	"github.com/Nahua-Foundation/ragota/internal/llm"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/storage"
	"github.com/Nahua-Foundation/ragota/internal/svcdetect"
)

// SetGenerator enables LLM summaries using the given text generator.
// maxFiles caps file summaries per repo per index run (<=0 means default 30);
// files turns the per-file and per-service passes on, independently of the
// symbol pass that shares this generator.
func (e *Enricher) SetGenerator(g llm.Generator, maxFiles int, files bool) {
	e.generator = g
	if maxFiles <= 0 {
		maxFiles = 30
	}
	e.maxSumm = maxFiles
	e.fileSummaries = files
}

// summarizableLanguages are code languages worth an LLM summary.
var summarizableLanguages = map[string]bool{
	"go": true, "java": true, "csharp": true, "typescript": true,
	"javascript": true, "python": true, "proto": true,
}

// SummarizeRepo generates file-level and service-level summaries for the
// files just indexed. Summaries are stored as units of kind "summary"
// (Meta scope "file" or "service", text in Doc).
func (e *Enricher) SummarizeRepo(ctx context.Context, repo *repos.Repo, files []*indexing.FileToIndex) error {
	if e.generator == nil || !e.fileSummaries || len(files) == 0 {
		return nil
	}

	svcs, _ := svcdetect.Detect(repo.Path, repo.Name)

	count := 0
	var errs []error
	var summaryDocs []*indexing.FileToIndex
	for _, f := range files {
		if !summarizableLanguages[f.Language] || len(f.Content) == 0 {
			continue
		}
		if count >= e.maxSumm {
			slog.Info("summary cap reached", "repo_id", repo.ID, "cap", e.maxSumm)
			break
		}
		content := f.Content
		if len(content) > 6000 {
			content = content[:6000]
		}
		prompt := "Summarize what this " + f.Language + " file does in 2-3 sentences. " +
			"Mention its main responsibilities and any external systems it talks to " +
			"(HTTP endpoints, gRPC services, Kafka topics, database tables).\n\n" +
			"File: " + f.Path + "\n\n" + string(content)
		text, err := e.generator.Generate(ctx, prompt)
		if err != nil {
			errs = append(errs, fmt.Errorf("summarize %s: %w", f.Path, err))
			continue
		}
		unit := &storage.ASTUnit{
			RepoID:    repo.ID,
			FilePath:  f.Path,
			Language:  f.Language,
			Kind:      storage.KindSummary,
			Name:      filepath.Base(f.Path),
			Qualified: "summary:" + repo.Name + "/" + f.Path,
			Signature: "file", // legacy convention, kept for readers of old data
			Doc:       strings.TrimSpace(text),
			Meta:      storage.EncodeUnitMeta(&storage.UnitMeta{Scope: "file"}),
			StartLine: 1,
			EndLine:   1,
			Hash:      f.Hash,
		}
		if err := e.storage.StoreASTUnit(ctx, unit); err != nil {
			errs = append(errs, err)
			continue
		}
		summaryDocs = append(summaryDocs, summaryDoc("file", unit.Name, unit.Doc))
		count++
	}

	svcDocs, err := e.summarizeServices(ctx, repo, svcs)
	if err != nil {
		errs = append(errs, err)
	}
	summaryDocs = append(summaryDocs, svcDocs...)

	// Make summaries findable via semantic search: feed them through the
	// vector indexer as synthetic markdown files under .ragota/summaries/.
	if err := e.indexSummaries(ctx, repo, summaryDocs); err != nil {
		errs = append(errs, err)
	}

	slog.Info("summaries generated", "repo_id", repo.ID, "files", count)
	return errors.Join(errs...)
}

// summaryDoc builds a synthetic markdown file for one summary. The first line
// ("summary: <scope> <name>") marks the vector points as summary-derived.
func summaryDoc(scope, name, text string) *indexing.FileToIndex {
	content := "summary: " + scope + " " + name + "\n\n" + text + "\n"
	return &indexing.FileToIndex{
		Path:     ".ragota/summaries/" + name + ".md",
		Language: "markdown",
		Content:  []byte(content),
	}
}

// indexSummaries pushes summary documents into the vector index so they are
// retrievable through semantic search. It is a no-op when no vector indexer
// is configured.
func (e *Enricher) indexSummaries(ctx context.Context, repo *repos.Repo, docs []*indexing.FileToIndex) error {
	if len(docs) == 0 {
		return nil
	}
	vecIdx, ok := e.indexers[indexing.IndexTypeVector]
	if !ok || e.storage.VectorStore() == nil {
		return nil
	}
	res, err := vecIdx.Index(ctx, &indexing.IndexRequest{
		RepoID:   repo.ID,
		RepoPath: repo.Path,
		RepoName: repo.Name,
		Files:    docs,
	})
	if err != nil {
		return fmt.Errorf("index summaries: %w", err)
	}
	if len(res.Errors) > 0 {
		slog.Warn("some summaries failed to vector-index",
			"repo_id", repo.ID, "failed", res.FilesFailed, "errors", res.Errors)
	}
	return nil
}

// summarizeServices rolls file summaries up into one summary per service.
// It returns the generated service summaries as synthetic documents for the
// vector index.
func (e *Enricher) summarizeServices(ctx context.Context, repo *repos.Repo, svcs []svcdetect.Service) ([]*indexing.FileToIndex, error) {
	fileSums, err := e.storage.GetASTUnits(ctx, storage.QueryOpts{RepoID: repo.ID, Kind: storage.KindSummary})
	if err != nil {
		return nil, err
	}
	byService := map[string][]string{}
	for _, u := range fileSums {
		meta := storage.DecodeUnitMeta(u.Meta)
		if meta.Scope != "file" && u.Signature != "file" {
			continue
		}
		svcName := svcdetect.ServiceFor(svcs, u.FilePath)
		if svcName == "" {
			svcName = repo.Name
		}
		byService[svcName] = append(byService[svcName], u.FilePath+": "+u.Doc)
	}

	var errs []error
	var docs []*indexing.FileToIndex
	for svcName, sums := range byService {
		joined := strings.Join(sums, "\n")
		if len(joined) > 8000 {
			joined = joined[:8000]
		}
		prompt := "Based on these per-file summaries, describe the service \"" + svcName +
			"\" in 3-4 sentences: its purpose, its API surface and what other systems it integrates with.\n\n" + joined
		text, gerr := e.generator.Generate(ctx, prompt)
		if gerr != nil {
			errs = append(errs, fmt.Errorf("summarize service %s: %w", svcName, gerr))
			continue
		}
		pseudoPath := ".ragota/summaries/" + svcName
		if derr := e.storage.DeleteASTUnitsByFile(ctx, repo.ID, pseudoPath); derr != nil {
			errs = append(errs, derr)
			continue
		}
		unit := &storage.ASTUnit{
			RepoID:    repo.ID,
			FilePath:  pseudoPath,
			Kind:      storage.KindSummary,
			Name:      svcName,
			Qualified: "summary:service:" + repo.Name + "/" + svcName,
			Signature: "service", // legacy convention, kept for readers of old data
			Doc:       strings.TrimSpace(text),
			Meta:      storage.EncodeUnitMeta(&storage.UnitMeta{Scope: "service"}),
			StartLine: 1,
			EndLine:   1,
		}
		if serr := e.storage.StoreASTUnit(ctx, unit); serr != nil {
			errs = append(errs, serr)
			continue
		}
		docs = append(docs, summaryDoc("service", svcName, unit.Doc))
	}
	return docs, errors.Join(errs...)
}
