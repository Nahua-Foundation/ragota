package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"ragota/internal/repos"
)

// clientKey — составной ключ кэша LSP-клиентов. Позволяет держать
// отдельный процесс LSP per (repo, language, workspaceRoot) — в multi-repo
// или multi-module workspace каждая репа/модуль получает свой сервер
// с rootURI = repo.Path или workspaceRoot.
type clientKey struct {
	repo   string // имя репы (из repos.Resolver); пусто = legacy/single-repo
	lang   string
	wsRoot string // workspace root (путь к go.mod/package.json и т.д.)
}

// Manager управляет LSP-клиентами per (repo, language). Ленивая
// инициализация: процесс стартует при первом обращении.
type Manager struct {
	root  string
	specs map[string]ServerSpec

	mu       sync.Mutex
	clients  map[clientKey]*Client
	resolver *repos.Resolver
}

// NewManager создаёт менеджер. Если specs == nil — берутся DefaultServers.
func NewManager(root string, specs []ServerSpec) *Manager {
	if specs == nil {
		specs = DefaultServers()
	}
	m := &Manager{
		root:    root,
		specs:   make(map[string]ServerSpec, len(specs)),
		clients: make(map[clientKey]*Client),
	}
	for _, s := range specs {
		m.specs[s.Language] = s
	}
	return m
}

// SetRepoResolver настраивает резолвер репо для multi-repo workspace.
// Если задан — EnsureOpen поднимает отдельный LSP-инстанс на каждую репу.
func (m *Manager) SetRepoResolver(r *repos.Resolver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resolver = r
}

// Languages возвращает список настроенных языков.
func (m *Manager) Languages() []string {
	out := make([]string, 0, len(m.specs))
	for k := range m.specs {
		out = append(out, k)
	}
	return out
}

// Get возвращает или запускает клиент для языка без привязки к репе.
// Сохранено для обратной совместимости. Эквивалентно GetForRepo(ctx, "", language, "").
func (m *Manager) Get(ctx context.Context, language string) (*Client, error) {
	return m.GetForRepo(ctx, "", language, "")
}

// GetWithRoot — старая сигнатура (без repo). Эквивалентно GetForRepo с repo="".
func (m *Manager) GetWithRoot(ctx context.Context, language, workspaceRoot string) (*Client, error) {
	return m.GetForRepo(ctx, "", language, workspaceRoot)
}

// GetForRepo возвращает или запускает клиента (repo, language) с указанным
// workspace root. Ключ кэша — тройка (repo, language, workspaceRoot); это
// позволяет держать одновременно несколько LSP одного языка для разных
// репо или подпроектов (каждый go.mod/package.json — свой процесс).
func (m *Manager) GetForRepo(ctx context.Context, repo, language, workspaceRoot string) (*Client, error) {
	key := clientKey{repo: repo, lang: language, wsRoot: workspaceRoot}
	m.mu.Lock()
	if c, ok := m.clients[key]; ok {
		if c.IsAlive() {
			lspDebug("LSP %s/%s: reusing existing client (alive, root %q)\n", repo, language, c.localRoot)
			m.mu.Unlock()
			return c, nil
		}
		lspDebug("LSP %s/%s: client dead, recreating\n", repo, language)
		_ = c.Close()
		delete(m.clients, key)
	}
	spec, ok := m.specs[language]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("no LSP configured for language %q", language)
	}
	m.mu.Unlock()

	root := m.root
	if workspaceRoot != "" {
		root = workspaceRoot
	}
	lspDebug("LSP %s/%s: starting new client (root=%q)\n", repo, language, root)
	c, err := Start(ctx, spec, root)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.clients[key] = c
	m.mu.Unlock()
	lspDebug("LSP %s/%s: client started\n", repo, language)
	return c, nil
}

// Close завершает все клиенты.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		_ = c.Close()
	}
	m.clients = make(map[clientKey]*Client)
}

// resolveRepo возвращает (repoName, repoPath) для абсолютного пути файла.
// Если резолвер не задан или путь не подпадает под известные репы —
// возвращает пустые строки.
func (m *Manager) resolveRepo(abs string) (string, string) {
	m.mu.Lock()
	r := m.resolver
	m.mu.Unlock()
	if r == nil {
		return "", ""
	}
	name := r.For(abs)
	if name == "" {
		return "", ""
	}
	for _, rp := range r.All() {
		if rp.Name == name {
			return name, rp.Path
		}
	}
	return name, ""
}

// findWorkspaceRoot ищет ближайший корень workspace вверх по дереву от файла.
// Для Go: go.mod, для Java: pom.xml/build.gradle, для Python: pyproject.toml/setup.py,
// для TypeScript: package.json/tsconfig.json.
func findWorkspaceRoot(startPath, language string) string {
	dir := filepath.Dir(startPath)
	markers := map[string][]string{
		"go":         {"go.mod"},
		"java":       {"pom.xml", "build.gradle", "build.gradle.kts"},
		"python":     {"pyproject.toml", "setup.py", "requirements.txt"},
		"typescript": {"package.json", "tsconfig.json"},
		"javascript": {"package.json"},
	}
	markerList, ok := markers[language]
	if !ok {
		return ""
	}

	for dir != "" && dir != "/" && len(dir) > 1 {
		for _, marker := range markerList {
			path := filepath.Join(dir, marker)
			if _, err := os.Stat(path); err == nil {
				lspDebug("LSP %s: findWorkspaceRoot: found %q for %q\n", language, path, startPath)
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	lspDebug("LSP %s: findWorkspaceRoot: NOT FOUND for %q (searched from %q)\n", language, startPath, filepath.Dir(startPath))
	return ""
}

// EnsureOpen открывает документ в LSP-клиенте, читая его с диска.
//
// Резолвинг клиента:
//  1. Если задан repos.Resolver и файл попадает в известную репу — клиент
//     ключуется по (repo, language), rootURI = repo.Path.
//  2. Иначе fallback на findWorkspaceRoot (go.mod/pom.xml/...) и затем
//     на общий m.root. Ключ репы при этом — пустая строка.
func (m *Manager) EnsureOpen(ctx context.Context, language, path string) (*Client, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(path)

	repoName, repoPath := m.resolveRepo(abs)

	// Выбираем workspaceRoot. Приоритет:
	// 1) путь самой репы (multi-repo);
	// 2) маркер языка (go.mod и т.п.) внутри этой репы или единственного workspace;
	// 3) общий m.root.
	workspaceRoot := repoPath
	if workspaceRoot == "" {
		workspaceRoot = findWorkspaceRoot(abs, language)
	}
	if workspaceRoot == "" {
		workspaceRoot = m.root
	}

	c, err := m.GetForRepo(ctx, repoName, language, workspaceRoot)
	if err != nil {
		return nil, err
	}

	lspDebug("LSP %s/%s: EnsureOpen: path=%q abs=%q workspaceRoot=%q client.rootURI=%q\n",
		repoName, language, path, abs, workspaceRoot, c.rootURI)

	// Нормализуем путь относительно workspace root.
	relPath := abs
	if workspaceRoot != "" {
		if rel, err := filepath.Rel(workspaceRoot, abs); err == nil && !strings.HasPrefix(rel, "..") {
			relPath = filepath.Join(workspaceRoot, rel)
		}
	}

	if err := c.DidOpen(relPath, language, string(data)); err != nil {
		return nil, err
	}
	return c, nil
}
