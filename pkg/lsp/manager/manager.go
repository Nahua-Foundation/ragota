// Package manager реализует кэш LSP-клиентов per (repo, language).
// Ленивая инициализация: процесс стартует при первом обращении.
package manager

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"ragota/pkg/fileutil"
	"ragota/pkg/lsp"
	"ragota/pkg/lsp/lifecycle"
	"ragota/pkg/repos"
)

// clientKey — составной ключ кэша LSP-клиентов.
type clientKey struct {
	repo   string
	lang   string
	wsRoot string
}

// Manager manages LSP clients per (repo, language). Lazy initialization.
type Manager struct {
	root  string
	specs map[string]lsp.ServerSpec

	mu       sync.Mutex
	clients  map[clientKey]lsp.Client
	resolver *repos.Resolver
}

// NewManager creates a new Manager. If specs == nil, uses DefaultServers.
func NewManager(root string, specs []lsp.ServerSpec) *Manager {
	if specs == nil {
		specs = lsp.DefaultServers()
	}
	m := &Manager{
		root:    root,
		specs:   make(map[string]lsp.ServerSpec, len(specs)),
		clients: make(map[clientKey]lsp.Client),
	}
	for _, s := range specs {
		m.specs[s.Language] = s
	}
	return m
}

// SetRepoResolver configures the repo resolver for multi-repo workspaces.
func (m *Manager) SetRepoResolver(r *repos.Resolver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resolver = r
}

// Languages returns the list of configured languages.
func (m *Manager) Languages() []string {
	langs := make([]string, 0, len(m.specs))
	for l := range m.specs {
		langs = append(langs, l)
	}
	return langs
}

// GetForRepo returns (or lazily starts) an LSP client for the given repo and language.
func (m *Manager) GetForRepo(ctx context.Context, repoPath, language string) (lsp.Client, error) {
	spec, ok := m.specs[language]
	if !ok {
		return nil, fmt.Errorf("lsp: no server configured for %s", language)
	}

	wsRoot := repoPath
	if wsRoot == "" {
		wsRoot = m.root
	}

	key := clientKey{
		repo:   filepath.Base(repoPath),
		lang:   language,
		wsRoot: wsRoot,
	}

	m.mu.Lock()
	if c, ok := m.clients[key]; ok && c.IsAlive() {
		m.mu.Unlock()
		return c, nil
	}
	// Clean up dead client if present
	if c, ok := m.clients[key]; ok {
		_ = c.Close()
		delete(m.clients, key)
	}
	m.mu.Unlock()

	// Start new client outside the lock (can be slow)
	c, err := lifecycle.Start(ctx, spec, wsRoot)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	// Double-check: another goroutine might have started one while we were launching
	if existing, ok := m.clients[key]; ok && existing.IsAlive() {
		m.mu.Unlock()
		_ = c.Close()
		return existing, nil
	}
	m.clients[key] = c
	m.mu.Unlock()

	return c, nil
}

// EnsureOpen ensures the file is opened in the appropriate LSP client.
// Returns the client for further operations.
func (m *Manager) EnsureOpen(ctx context.Context, path string) (lsp.Client, error) {
	lang := languageFromExt(filepath.Ext(path))
	if lang == "" {
		return nil, nil
	}

	repoPath := m.root
	if m.resolver != nil {
		if repo := m.resolver.For(path); repo != "" {
			repoPath = repo
		}
	}

	c, err := m.GetForRepo(ctx, repoPath, lang)
	if err != nil {
		return nil, err
	}
	return c, c.EnsureOpen(ctx, path)
}

// Close shuts down all LSP clients.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, c := range m.clients {
		_ = c.Close()
		delete(m.clients, key)
	}
}

// languageFromExt maps file extension to language code.
// Delegates to fileutil.LanguageByExt for a single source of truth.
func languageFromExt(ext string) string {
	lang := fileutil.LanguageByExt(ext)
	// LSP servers only support go, typescript, javascript, python, java.
	switch lang {
	case "go", "typescript", "javascript", "python", "java":
		return lang
	}
	return ""
}
