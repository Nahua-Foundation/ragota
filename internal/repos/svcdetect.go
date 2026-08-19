// Service detection: finding the deployable services inside a repository.
//
// Detection sources, in priority order:
//  1. .ragota.yaml manifest at the repo root (explicit override)
//  2. docker-compose.yml / compose.yaml services with build contexts
//  3. cmd/<name> directories containing main.go (Go convention)
//  4. per-directory build manifests: Dockerfile, pom.xml, build.gradle,
//     *.csproj, package.json, go.mod
//
// A repository with no nested service markers becomes a single service rooted
// at the repository root.
//
// The manifest file itself is read via LoadManifest, which also carries the
// repository's ignore patterns; this file only reads its services section.
package repos

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Service is a detected deployable unit inside a repository.
type Service struct {
	Name       string // service name (directory or manifest-declared)
	Root       string // repo-relative root directory, "" for repo root
	Manifest   string // repo-relative path of the file that identified the service
	DetectedBy string // "ragota.yaml" | "docker-compose" | "cmd" | "manifest" | "root"
}

// composeFile is the subset of docker-compose we care about.
type composeFile struct {
	Services map[string]struct {
		Build any `yaml:"build"` // string context or {context: string}
	} `yaml:"services"`
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "target": true,
	"bin": true, "obj": true, "dist": true, ".idea": true, ".vscode": true,
}

// maxDepth caps how deep the manifest walk descends, counted in directories
// below the repository root. It must clear the deepest realistic service
// layout — apps/backend/services/orders/api/pom.xml sits five directories
// down — while still cutting off pathological trees.
const maxDepth = 6

// DetectServices returns the services of the repository at repoPath, using repoName
// for single-service repositories.
func DetectServices(repoPath, repoName string) ([]Service, error) {
	// 1. Explicit manifest wins outright.
	if svcs := detectRagotaYAML(repoPath); len(svcs) > 0 {
		return svcs, nil
	}

	byRoot := map[string]Service{}
	add := func(s Service) {
		s.Root = filepath.ToSlash(strings.Trim(s.Root, "/"))
		if _, exists := byRoot[s.Root]; !exists {
			byRoot[s.Root] = s
		}
	}

	// 2. docker-compose services.
	for _, s := range detectCompose(repoPath) {
		add(s)
	}

	// 3. cmd/<name>/main.go convention.
	for _, s := range detectCmdDirs(repoPath) {
		add(s)
	}

	// 4. Per-directory build manifests. Root-level manifests do not name a
	// nested service; the single-service fallback below covers that case.
	for _, s := range detectManifests(repoPath) {
		if s.Root == "" {
			continue
		}
		add(s)
	}

	if len(byRoot) == 0 {
		byRoot[""] = Service{Name: repoName, Root: "", DetectedBy: "root"}
	}

	var out []Service
	for _, s := range byRoot {
		out = append(out, s)
	}
	disambiguateNames(out)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// maxNameDepth bounds how many root components a name may absorb while being
// disambiguated.
const maxNameDepth = 4

// disambiguateNames makes service names unique within a repository by
// qualifying repeats with their parent directories: services/orders/api and
// services/users/api are two services named "api", and every consumer of a
// service graph keys links on the name, so leaving both as "api" merges them
// into one node with the edges of both.
func disambiguateNames(svcs []Service) {
	for depth := 2; depth <= maxNameDepth; depth++ {
		counts := map[string]int{}
		for _, s := range svcs {
			counts[s.Name]++
		}
		changed := false
		for i, s := range svcs {
			if counts[s.Name] < 2 {
				continue
			}
			name := rootTail(s.Root, depth)
			if name == "" || name == s.Name {
				continue // rooted at the repo root, or already fully qualified
			}
			svcs[i].Name = name
			changed = true
		}
		if !changed {
			return
		}
	}
}

// rootTail returns the last n components of a repo-relative root, joined with
// "/": rootTail("services/orders/api", 2) == "orders/api".
func rootTail(root string, n int) string {
	root = strings.Trim(filepath.ToSlash(root), "/")
	if root == "" {
		return ""
	}
	parts := strings.Split(root, "/")
	if n < len(parts) {
		parts = parts[len(parts)-n:]
	}
	return strings.Join(parts, "/")
}

// detectRagotaYAML returns the services the repository declares for itself. A
// malformed manifest falls back to the heuristics but says so: the same file
// carries the ignore patterns, and a typo there changes what gets indexed.
func detectRagotaYAML(repoPath string) []Service {
	m, err := LoadManifest(repoPath)
	if err != nil {
		slog.Warn("repository manifest", "path", repoPath, "err", err)
		return nil
	}
	if m == nil {
		return nil
	}
	var out []Service
	for _, s := range m.Services {
		out = append(out, Service{
			Name: s.Name, Root: s.Root,
			Manifest: m.File, DetectedBy: "ragota.yaml",
		})
	}
	return out
}

func detectCompose(repoPath string) []Service {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		var c composeFile
		if err := yaml.Unmarshal(data, &c); err != nil {
			continue
		}
		var out []Service
		for svcName, svc := range c.Services {
			root := composeBuildContext(svc.Build)
			if root == "" {
				continue // image-only service (db, broker) — not source code
			}
			out = append(out, Service{
				Name: svcName, Root: root, Manifest: name, DetectedBy: "docker-compose",
			})
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func composeBuildContext(build any) string {
	switch b := build.(type) {
	case string:
		return cleanRel(b)
	case map[string]any:
		if ctx, ok := b["context"].(string); ok {
			return cleanRel(ctx)
		}
	}
	return ""
}

func cleanRel(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	p = strings.TrimPrefix(p, "./")
	if p == "." {
		return ""
	}
	return strings.Trim(p, "/")
}

func detectCmdDirs(repoPath string) []Service {
	entries, err := os.ReadDir(filepath.Join(repoPath, "cmd"))
	if err != nil {
		return nil
	}
	var out []Service
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mainPath := filepath.Join(repoPath, "cmd", e.Name(), "main.go")
		if _, err := os.Stat(mainPath); err == nil {
			out = append(out, Service{
				Name:       e.Name(),
				Root:       "cmd/" + e.Name(),
				Manifest:   "cmd/" + e.Name() + "/main.go",
				DetectedBy: "cmd",
			})
		}
	}
	return out
}

var manifestNames = map[string]bool{
	"Dockerfile": true, "pom.xml": true, "build.gradle": true,
	"build.gradle.kts": true, "package.json": true, "go.mod": true,
}

func isManifest(name string) bool {
	return manifestNames[name] || strings.HasSuffix(name, ".csproj")
}

func detectManifests(repoPath string) []Service {
	var out []Service
	_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(repoPath, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[d.Name()] || strings.Count(rel, "/") >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !isManifest(d.Name()) {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			dir = ""
		}
		name := filepath.Base(dir)
		if dir == "" {
			name = ""
		}
		out = append(out, Service{
			Name: name, Root: dir, Manifest: rel, DetectedBy: "manifest",
		})
		return nil
	})
	return out
}

// MergeServiceHints merges LLM-provided service hints into detector results.
// Detected services win: a hint is added only when no detected service
// already claims its root. Added hints are marked DetectedBy "llm".
func MergeServiceHints(detected []Service, hints []Service) []Service {
	out := append([]Service(nil), detected...)
	taken := make(map[string]bool, len(detected))
	for _, s := range detected {
		taken[filepath.ToSlash(strings.Trim(s.Root, "/"))] = true
	}
	for _, h := range hints {
		if h.Name == "" {
			continue
		}
		root := filepath.ToSlash(strings.Trim(h.Root, "/"))
		if taken[root] {
			continue
		}
		taken[root] = true
		h.Root = root
		h.DetectedBy = "llm"
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ServiceFor returns the name of the service owning a repo-relative file path,
// picking the service with the longest matching root prefix.
func ServiceFor(services []Service, filePath string) string {
	filePath = filepath.ToSlash(filePath)
	best := ""
	bestLen := -1
	for _, s := range services {
		if s.Root == "" {
			if bestLen < 0 {
				best, bestLen = s.Name, 0
			}
			continue
		}
		prefix := s.Root + "/"
		if (filePath == s.Root || strings.HasPrefix(filePath, prefix)) && len(s.Root) > bestLen {
			best, bestLen = s.Name, len(s.Root)
		}
	}
	return best
}
