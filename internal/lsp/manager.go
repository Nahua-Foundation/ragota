package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Manager управляет LSP-клиентами на язык. Ленивая инициализация:
// процесс стартует при первом обращении к языку.
type Manager struct {
	root  string
	specs map[string]ServerSpec

	mu      sync.Mutex
	clients map[string]*Client
	roots   map[string]string // language -> workspace root (кэшируем найденный root)
}

// NewManager создаёт менеджер. Если specs == nil — берутся DefaultServers.
func NewManager(root string, specs []ServerSpec) *Manager {
	if specs == nil {
		specs = DefaultServers()
	}
	m := &Manager{
		root:    root,
		specs:   make(map[string]ServerSpec, len(specs)),
		clients: make(map[string]*Client),
		roots:   make(map[string]string),
	}
	for _, s := range specs {
		m.specs[s.Language] = s
	}
	return m
}

// Languages возвращает список настроенных языков.
func (m *Manager) Languages() []string {
	out := make([]string, 0, len(m.specs))
	for k := range m.specs {
		out = append(out, k)
	}
	return out
}

// Get возвращает или запускает клиент для языка.
func (m *Manager) Get(ctx context.Context, language string) (*Client, error) {
	return m.GetWithRoot(ctx, language, "")
}

// GetWithRoot возвращает или запускает клиент с указанным workspace root.
func (m *Manager) GetWithRoot(ctx context.Context, language, workspaceRoot string) (*Client, error) {
	m.mu.Lock()
	if c, ok := m.clients[language]; ok {
		if c.IsAlive() && (workspaceRoot == "" || samePath(c.localRoot, workspaceRoot)) {
			lspDebug("LSP %s: reusing existing client (alive, same root %q)\n", language, c.localRoot)
			m.mu.Unlock()
			return c, nil
		}
		// Клиент умер или корень изменился, удаляем из мапы
		lspDebug("LSP %s: client dead or root changed (%q -> %q), recreating\n", language, c.localRoot, workspaceRoot)
		_ = c.Close()
		delete(m.clients, language)
	}
	spec, ok := m.specs[language]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("no LSP configured for language %q", language)
	}
	m.mu.Unlock()

	// Используем workspace root если указан, иначе общий root
	root := m.root
	if workspaceRoot != "" {
		root = workspaceRoot
	}
	lspDebug("LSP %s: starting new client (root=%q)\n", language, root)
	c, err := Start(ctx, spec, root)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.clients[language] = c
	m.mu.Unlock()
	lspDebug("LSP %s: client started\n", language)
	return c, nil
}

// Close завершает все клиенты.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		_ = c.Close()
	}
	m.clients = make(map[string]*Client)
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
// Путь нормализуется относительно workspace root (ищет go.mod/pom.xml и т.д.).
func (m *Manager) EnsureOpen(ctx context.Context, language, path string) (*Client, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(path)

	// Ищем workspace root для языка ДО создания клиента
	workspaceRoot := findWorkspaceRoot(abs, language)
	if workspaceRoot == "" {
		workspaceRoot = m.root
	}

	// Создаём или получаем клиент с правильным root
	c, err := m.GetWithRoot(ctx, language, workspaceRoot)
	if err != nil {
		return nil, err
	}

	lspDebug("LSP %s: EnsureOpen: path=%q abs=%q workspaceRoot=%q client.rootURI=%q\n",
		language, path, abs, workspaceRoot, c.rootURI)

	// Нормализуем путь относительно workspace root или общего root
	relPath := abs
	if workspaceRoot != "" {
		// Если нашли маркер (go.mod и т.д.), используем его как root
		if rel, err := filepath.Rel(workspaceRoot, abs); err == nil && !strings.HasPrefix(rel, "..") {
			relPath = filepath.Join(workspaceRoot, rel)
			lspDebug("LSP %s: EnsureOpen: normalized to workspace: %q\n", language, relPath)
		}
	} else if rel, err := filepath.Rel(m.root, abs); err == nil && !strings.HasPrefix(rel, "..") {
		relPath = filepath.Join(m.root, rel)
		lspDebug("LSP %s: EnsureOpen: normalized to manager root: %q\n", language, relPath)
	}

	if err := c.DidOpen(relPath, language, string(data)); err != nil {
		return nil, err
	}
	return c, nil
}
