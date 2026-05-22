package lsp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Manager управляет LSP-клиентами на язык. Ленивая инициализация:
// процесс стартует при первом обращении к языку.
type Manager struct {
	root  string
	specs map[string]ServerSpec

	mu      sync.Mutex
	clients map[string]*Client
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
	m.mu.Lock()
	if c, ok := m.clients[language]; ok {
		if c.IsAlive() {
			m.mu.Unlock()
			return c, nil
		}
		// Клиент умер, удаляем из мапы
		delete(m.clients, language)
	}
	spec, ok := m.specs[language]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("no LSP configured for language %q", language)
	}
	m.mu.Unlock()

	if _, err := exec.LookPath(spec.Command); err != nil {
		return nil, fmt.Errorf("LSP binary %q for %s not found in PATH", spec.Command, language)
	}
	c, err := Start(ctx, spec, m.root)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.clients[language] = c
	m.mu.Unlock()
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

// EnsureOpen открывает документ в LSP-клиенте, читая его с диска.
func (m *Manager) EnsureOpen(ctx context.Context, language, path string) (*Client, error) {
	c, err := m.Get(ctx, language)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(path)
	if err := c.DidOpen(abs, language, string(data)); err != nil {
		return nil, err
	}
	return c, nil
}
