package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/repos"
)

// Source is a Git repository source.
type Source struct {
	workDir string
	auth    *Auth
	repos   map[string]*domain.Repo // repo ID -> repo tracking
	reposMu sync.RWMutex
}

// Auth holds Git authentication credentials.
type Auth struct {
	GitHubToken string
	GitLabToken string
}

// Config is the Git source configuration.
type Config struct {
	WorkDir string
	Auth    *Auth
}

// New creates a new Git source.
func New(cfg *Config) *Source {
	if cfg == nil {
		cfg = &Config{}
	}
	return &Source{
		workDir: cfg.WorkDir,
		auth:    cfg.Auth,
		repos:   make(map[string]*domain.Repo),
	}
}

// Name returns the source name.
func (s *Source) Name() string {
	return "git"
}

// Type returns the source type.
func (s *Source) Type() domain.SourceType {
	return domain.SourceTypeGit
}

// Init initializes the Git source.
func (s *Source) Init(ctx context.Context, config map[string]interface{}) error {
	// Configuration is passed via New
	return nil
}

// Add adds a Git repository by cloning it.
func (s *Source) Add(ctx context.Context, req *domain.AddRequest) (*domain.Repo, error) {
	if req.URL == "" {
		return nil, fmt.Errorf("url is required for git source")
	}
	if err := validateRemoteURL(req.URL); err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if err := validateBranch(req.Branch); err != nil {
		return nil, fmt.Errorf("invalid branch: %w", err)
	}

	// Expand work directory
	workDir := s.workDir
	if workDir == "" {
		workDir = "~/.ragota/repos"
	}
	workDir = config.ExpandPath(workDir)

	// Create repo directory
	repoDir := filepath.Join(workDir, sanitizeName(req.Name))
	if err := os.MkdirAll(filepath.Dir(repoDir), 0755); err != nil {
		return nil, fmt.Errorf("create directory: %w", err)
	}

	// Clone the repository
	branch := req.Branch
	if branch == "" {
		branch = s.detectDefaultBranch(ctx, req.URL)
		if branch == "" {
			branch = "main" // fallback
		}
	}

	// Clone into a sibling temp directory and rename on success. A clone that
	// is interrupted (cancelled request, killed process) would otherwise leave
	// a partial repoDir behind, and every retry then fails for good with git's
	// "destination path already exists".
	if err := s.cloneAtomic(ctx, req.URL, repoDir, branch); err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}

	repo := &domain.Repo{
		ID:        repos.GenerateID(req.Name, req.URL),
		Name:      req.Name,
		Source:    domain.SourceTypeGit,
		URL:       req.URL,
		Path:      repoDir,
		Branch:    branch,
		Status:    domain.StatusIdle,
		CreatedAt: 0, // Set by caller
	}

	// Track the repository
	s.reposMu.Lock()
	s.repos[repo.ID] = repo
	s.reposMu.Unlock()

	return repo, nil
}

// Remove removes a repository by deleting its cloned files.
func (s *Source) Remove(ctx context.Context, repoID string) error {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()

	repo, ok := s.repos[repoID]
	if !ok {
		return fmt.Errorf("repository %s not found", repoID)
	}

	// Delete the directory
	if err := os.RemoveAll(repo.Path); err != nil {
		return fmt.Errorf("delete repository directory: %w", err)
	}

	// Remove from tracking
	delete(s.repos, repoID)

	return nil
}

// GetRepo retrieves a tracked repository by ID.
func (s *Source) GetRepo(repoID string) (*domain.Repo, bool) {
	s.reposMu.RLock()
	defer s.reposMu.RUnlock()
	repo, ok := s.repos[repoID]
	return repo, ok
}

// ListRepos returns all tracked repositories.
func (s *Source) ListRepos() []*domain.Repo {
	s.reposMu.RLock()
	defer s.reposMu.RUnlock()
	list := make([]*domain.Repo, 0, len(s.repos))
	for _, repo := range s.repos {
		list = append(list, repo)
	}
	return list
}

// Update updates a repository by pulling latest changes.
func (s *Source) Update(ctx context.Context, repo *domain.Repo) error {
	return s.pull(ctx, repo.URL, repo.Path)
}

// GetFiles returns files in a repository for index. The walk itself is
// shared with the local source.
func (s *Source) GetFiles(ctx context.Context, repo *domain.Repo, ignorePatterns []string) ([]*domain.RepoFile, error) {
	return repos.WalkFiles(repo.Path, ignorePatterns)
}

// Clean removes repository files from disk.
func (s *Source) Clean(ctx context.Context, repo *domain.Repo) error {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()

	// Delete the directory
	if err := os.RemoveAll(repo.Path); err != nil {
		return fmt.Errorf("delete repository directory: %w", err)
	}

	// Remove from tracking
	delete(s.repos, repo.ID)

	return nil
}

// Close closes the source.
func (s *Source) Close() error {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	// Clear tracking
	s.repos = make(map[string]*domain.Repo)
	return nil
}

// clone clones a Git repository. The "--" separates the caller-supplied URL and
// directory from the option list so that a URL beginning with a dash cannot be
// read as a git flag (the --upload-pack= remote-code-execution class).
func (s *Source) clone(ctx context.Context, url, dir, branch string) error {
	args := []string{"clone", "--depth", "1", "--branch", branch, "--", url, dir}
	token := s.getTokenForURL(url)
	return s.runGit(ctx, nil, token, args...)
}

// cloneAtomic clones into a temporary directory next to dir and moves it into
// place only once the clone succeeded, so a failed or interrupted clone leaves
// nothing behind for the next attempt to trip over.
func (s *Source) cloneAtomic(ctx context.Context, url, dir, branch string) error {
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("destination %s already exists", dir)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".clone-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	// git refuses to clone into a non-empty directory, and MkdirTemp created
	// it; hand git a path it may create itself.
	target := filepath.Join(tmp, "repo")
	defer func() { _ = os.RemoveAll(tmp) }()

	if err := s.clone(ctx, url, target, branch); err != nil {
		return err
	}
	if err := os.Rename(target, dir); err != nil {
		return fmt.Errorf("move clone into place: %w", err)
	}
	return nil
}

// pull pulls latest changes. It authenticates with the same token clone used:
// without it a private repository clones once (Add carries the token) and then
// every subsequent Update fails, so the mirror silently stops tracking.
func (s *Source) pull(ctx context.Context, url, dir string) error {
	args := []string{"-C", dir, "pull"}
	return s.runGit(ctx, nil, s.getTokenForURL(url), args...)
}

// getTokenForURL returns the appropriate token for a git URL.
func (s *Source) getTokenForURL(url string) string {
	if s.auth == nil {
		return ""
	}
	if strings.Contains(url, "github.com") && s.auth.GitHubToken != "" {
		return s.auth.GitHubToken
	}
	if strings.Contains(url, "gitlab.com") && s.auth.GitLabToken != "" {
		return s.auth.GitLabToken
	}
	return ""
}

// detectDefaultBranch attempts to detect the default branch of a repository.
func (s *Source) detectDefaultBranch(ctx context.Context, url string) string {
	// Try to get the default branch without cloning
	// This uses the git ls-remote command. "--" guards the URL as in clone.
	args := []string{"ls-remote", "--symref", "--", url, "HEAD"}
	cmd := exec.CommandContext(ctx, "git", args...)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	// Parse output to find the default branch. The symref line is
	// "ref: refs/heads/<branch>\tHEAD": everything after the marker up to the
	// next whitespace (the tab before HEAD) is the branch name.
	const marker = "ref: refs/heads/"
	for _, line := range strings.Split(string(output), "\n") {
		idx := strings.Index(line, marker)
		if idx == -1 {
			continue
		}
		branch := line[idx+len(marker):]
		if i := strings.IndexAny(branch, " \t"); i != -1 {
			branch = branch[:i]
		}
		if branch = strings.TrimSpace(branch); branch != "" {
			return branch
		}
	}

	return ""
}

// runGit runs a Git command with optional environment variables. When a token
// is given it is injected as an http.extraHeader through GIT_CONFIG_* in the
// environment rather than "-c" on the command line: an argument is world-visible
// via ps on a shared host, an environment variable of a child process is not.
// The token is only ever non-empty for the provider host it belongs to (see
// getTokenForURL), so it cannot leak onto a third-party remote.
func (s *Source) runGit(ctx context.Context, env []string, token string, args ...string) error {
	if env == nil {
		env = os.Environ()
	}
	if token != "" {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Bearer "+token,
		)
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env

	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := fmt.Sprintf("git %s: %v - %s", strings.Join(args, " "), err, string(output))
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "***")
		}
		return fmt.Errorf("%s", msg)
	}

	return nil
}

// Helper functions

// validateRemoteURL rejects URLs that git could misread as an option or that
// point at a local git helper. It is deliberately conservative: a leading dash
// turns a positional argument into a flag (the --upload-pack=<cmd> class of
// remote code execution — the "--" in clone/ls-remote is the second line of
// defence for git versions that honour it), whitespace has no place in a URL,
// and only the network transports this source is actually pointed at are
// allowed, so a crafted "remote" like file:// or ext:: cannot reach a helper.
func validateRemoteURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url is empty")
	}
	if strings.HasPrefix(raw, "-") {
		return fmt.Errorf("url may not begin with a dash")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return fmt.Errorf("url may not contain whitespace")
	}
	switch {
	case strings.HasPrefix(raw, "https://"),
		strings.HasPrefix(raw, "http://"),
		strings.HasPrefix(raw, "git://"),
		strings.HasPrefix(raw, "ssh://"):
		return nil
	case scpLikeURL(raw):
		return nil
	default:
		return fmt.Errorf("unsupported url transport (want http(s)/git/ssh or user@host:path)")
	}
}

// scpLikeURL reports whether raw is git's scp-like syntax "user@host:path" (or
// "host:path"), where the first colon precedes the first slash and no explicit
// transport scheme is present.
func scpLikeURL(raw string) bool {
	if strings.Contains(raw, "://") {
		return false
	}
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 {
		return false
	}
	slash := strings.IndexByte(raw, '/')
	return slash == -1 || colon < slash
}

// validateBranch rejects a branch name git could read as an option or that
// carries characters no ref may contain. An empty name means "detect the
// default", which the caller handles.
func validateBranch(branch string) error {
	if branch == "" {
		return nil
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("branch may not begin with a dash")
	}
	if strings.ContainsAny(branch, " \t\r\n\\~^:?*[") {
		return fmt.Errorf("branch contains invalid characters")
	}
	return nil
}

func sanitizeName(name string) string {
	// Simple sanitization - replace special chars with dash
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	return strings.TrimSpace(s)
}
