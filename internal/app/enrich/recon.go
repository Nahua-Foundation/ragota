package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/llm"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// KindRecon marks the unit that stores the raw LLM recon result for a repo
// (Qualified "recon:<repoID>", Doc = the assistant's JSON answer). Its
// presence means the recon pass already ran for the repo.
const KindRecon = "recon"

// ReconFilePath is the pseudo file path of the recon unit: recon output is
// stored as a unit, so it needs a path, and this is the one it carries.
const ReconFilePath = ".ragota/recon"

// SetReconAssistant enables the assistant LLM: a pre-index recon pass over
// the repository structure (recon) and/or LLM disambiguation of ambiguous
// contract edges in the linker (disambiguate).
func (e *Enricher) SetReconAssistant(gen llm.Generator, recon, disambiguate bool) {
	e.reconGen = gen
	e.reconEnabled = recon
	e.disambigEnabled = disambiguate
	if disambiguate && gen != nil {
		e.linker.SetDisambiguator(makeDisambiguator(gen))
	}
}

// makeDisambiguator wraps a text generator into the linker's choice hook:
// the reply is parsed as a single integer (candidate index; negative or
// unparsable replies decline the choice).
func makeDisambiguator(gen llm.Generator) func(ctx context.Context, prompt string) (int, bool) {
	return func(ctx context.Context, prompt string) (int, bool) {
		out, err := gen.Generate(ctx, prompt)
		if err != nil {
			slog.Warn("edge disambiguation generate", "err", err)
			return 0, false
		}
		n, err := parseChoice(out)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
}

// parseChoice extracts the first (possibly negative) integer from an LLM reply.
func parseChoice(s string) (int, error) {
	for i := 0; i < len(s); i++ {
		isDigit := s[i] >= '0' && s[i] <= '9'
		isNeg := s[i] == '-' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'
		if !isDigit && !isNeg {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		return strconv.Atoi(s[i:j])
	}
	return 0, fmt.Errorf("no integer in %q", s)
}

// reconService is one service entry of the assistant's recon answer.
type reconService struct {
	Name     string `json:"name"`
	Root     string `json:"root"`
	Purpose  string `json:"purpose"`
	Language string `json:"language"`
}

// reconResult is the required JSON shape of the assistant's recon answer.
type reconResult struct {
	Services    []reconService `json:"services"`
	ConfigPaths []string       `json:"config_paths"`
	Notes       string         `json:"notes"`
}

// ReconRepo runs the pre-index recon pass once per repository: it sends a
// compact repo overview (directory tree, README head, build manifests) to the
// assistant LLM and stores the raw JSON answer as a "recon" unit. Subsequent
// index runs skip the pass because the unit already exists.
func (e *Enricher) ReconRepo(ctx context.Context, repo *domain.Repo) error {
	existing, err := e.store.GetASTUnits(ctx, domain.QueryOpts{RepoID: repo.ID, Kind: KindRecon})
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil // recon already done for this repo
	}

	overview := buildReconOverview(repo.Path, repo.Name)
	out, err := e.reconGen.Generate(ctx, reconPrompt(overview))
	if err != nil {
		return fmt.Errorf("recon generate: %w", err)
	}
	raw := extractJSON(out)
	var res reconResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return fmt.Errorf("recon parse: %w", err)
	}

	unit := &domain.ASTUnit{
		RepoID:    repo.ID,
		FilePath:  ReconFilePath,
		Kind:      KindRecon,
		Name:      repo.Name,
		Qualified: "recon:" + repo.ID,
		Doc:       raw,
		Meta:      store.EncodeUnitMeta(&store.UnitMeta{DetectedBy: "llm"}),
		StartLine: 1,
		EndLine:   1,
	}
	if err := e.store.StoreASTUnit(ctx, unit); err != nil {
		return fmt.Errorf("store recon unit: %w", err)
	}
	reconTotal.Inc()
	slog.Info("recon pass done", "repo_id", repo.ID, "services", len(res.Services))
	return nil
}

// ReconHints loads the LLM service hints from the stored recon unit; nil when
// no recon was done or its answer is unusable.
func (e *Enricher) ReconHints(ctx context.Context, repo *domain.Repo) []repos.Service {
	units, err := e.store.GetASTUnits(ctx, domain.QueryOpts{RepoID: repo.ID, Kind: KindRecon})
	if err != nil || len(units) == 0 {
		return nil
	}
	var res reconResult
	if err := json.Unmarshal([]byte(units[0].Doc), &res); err != nil {
		return nil
	}
	var hints []repos.Service
	for _, sv := range res.Services {
		if sv.Name == "" {
			continue
		}
		hints = append(hints, repos.Service{
			Name:       sv.Name,
			Root:       sv.Root,
			Manifest:   ReconFilePath,
			DetectedBy: "llm",
		})
	}
	return hints
}

// Recon overview limits.
const (
	reconMaxDepth      = 3    // directory tree depth
	reconReadmeBytes   = 2048 // README head included in the overview
	reconOverviewBytes = 8192 // hard cap on the whole overview
)

// reconManifestNames are build manifests worth listing in the overview.
var reconManifestNames = map[string]bool{
	"go.mod": true, "pom.xml": true, "build.gradle": true,
	"build.gradle.kts": true, "package.json": true, "Dockerfile": true,
	"docker-compose.yml": true, "docker-compose.yaml": true,
	"compose.yml": true, "compose.yaml": true,
	".ragota.yaml": true, ".ragota.yml": true,
}

func isReconManifest(name string) bool {
	return reconManifestNames[name] || strings.HasSuffix(name, ".csproj")
}

var reconSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "target": true,
	"bin": true, "obj": true, "dist": true, ".idea": true, ".vscode": true,
}

// buildReconOverview composes the compact repository overview sent to the
// assistant: directory tree up to depth 3 (directories + build manifests),
// the first 2KB of the README and the manifest list, capped at 8KB total.
func buildReconOverview(repoPath, repoName string) string {
	var tree, manifests []string
	_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(repoPath, path)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if reconSkipDirs[d.Name()] || strings.Count(rel, "/") >= reconMaxDepth {
				return filepath.SkipDir
			}
			tree = append(tree, rel+"/")
			return nil
		}
		if isReconManifest(d.Name()) {
			tree = append(tree, rel)
			manifests = append(manifests, rel)
		}
		return nil
	})
	sort.Strings(tree)
	sort.Strings(manifests)

	var b strings.Builder
	b.WriteString("Repository: " + repoName + "\n\nDirectory tree (directories and build manifests, depth <= 3):\n")
	for _, line := range tree {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if readme := readReadmeHead(repoPath); readme != "" {
		b.WriteString("\nREADME (first 2KB):\n")
		b.WriteString(readme)
		b.WriteByte('\n')
	}
	if len(manifests) > 0 {
		b.WriteString("\nBuild manifests:\n")
		for _, m := range manifests {
			b.WriteString(m)
			b.WriteByte('\n')
		}
	}
	out := b.String()
	if len(out) > reconOverviewBytes {
		out = out[:reconOverviewBytes]
	}
	return out
}

// readReadmeHead returns the first reconReadmeBytes of the repo README, "" if none.
func readReadmeHead(repoPath string) string {
	for _, name := range []string{"README.md", "README", "README.rst", "Readme.md", "readme.md"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		if len(data) > reconReadmeBytes {
			data = data[:reconReadmeBytes]
		}
		return strings.TrimSpace(string(data))
	}
	return ""
}

// reconPrompt is the recon instruction for the assistant LLM. The answer must
// be a bare JSON object.
func reconPrompt(overview string) string {
	return "You are analyzing a source code repository before it is indexed.\n\n" +
		overview + "\n\n" +
		"Identify the deployable services of this repository. For each service give " +
		"its name, its repo-relative root directory (\"\" for the repository root), " +
		"a one-line purpose and its main programming language. Also list the paths of " +
		"configuration files likely to contain message topics, queues or service URLs.\n\n" +
		"Respond with ONLY this JSON object, no prose, no markdown fences:\n" +
		`{"services":[{"name":"...","root":"...","purpose":"...","language":"..."}],"config_paths":["..."],"notes":"..."}`
}

// extractJSON returns the JSON object inside s, tolerating ```json fences and
// surrounding prose: the substring from the first '{' to the last '}'.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return strings.TrimSpace(s)
	}
	return s[start : end+1]
}
