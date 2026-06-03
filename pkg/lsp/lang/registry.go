// Package lang содержит language-specific capabilities, initialization options
// и конфигурации для поддерживаемых LSP-серверов.
package lang

// Capabilities описывает настройки для конкретного языка.
type Capabilities struct {
	// ClientCapabilities возвращает capabilities для initialize запроса.
	ClientCapabilities func() map[string]any
	// InitOptions возвращает initializationOptions.
	InitOptions func() map[string]any
	// ConfigFor возвращает настройки для workspace/configuration.
	ConfigFor func(section string) any
}

// Registry — реестр capabilities по языку.
type Registry struct {
	langs map[string]Capabilities
}

// Default возвращает реестр со всеми поддерживаемыми языками.
func Default() *Registry {
	r := &Registry{langs: make(map[string]Capabilities)}
	r.Register("go", Capabilities{
		ClientCapabilities: GoCapabilities,
		InitOptions:        GoInitOptions,
		ConfigFor:          GoConfigFor,
	})
	r.Register("java", Capabilities{
		ClientCapabilities: JavaCapabilities,
		InitOptions:        JavaInitOptions,
		ConfigFor:          JavaConfigFor,
	})
	r.Register("python", Capabilities{
		ClientCapabilities: PythonCapabilities,
		InitOptions:        PythonInitOptions,
		ConfigFor:          PythonConfigFor,
	})
	r.Register("typescript", Capabilities{
		ClientCapabilities: TypeScriptCapabilities,
		InitOptions:        TypeScriptInitOptions,
		ConfigFor:          TypeScriptConfigFor,
	})
	// javascript uses the same capabilities as typescript
	r.Register("javascript", Capabilities{
		ClientCapabilities: TypeScriptCapabilities,
		InitOptions:        TypeScriptInitOptions,
		ConfigFor:          TypeScriptConfigFor,
	})
	return r
}

// Register adds or replaces capabilities for a language.
func (r *Registry) Register(language string, caps Capabilities) {
	r.langs[language] = caps
}

// Get returns capabilities for a language. Returns nil if not registered.
func (r *Registry) Get(language string) *Capabilities {
	c, ok := r.langs[language]
	if !ok {
		return nil
	}
	return &c
}

// DefaultCapabilities returns generic capabilities for unregistered languages.
func DefaultCapabilities() map[string]any {
	return map[string]any{
		"textDocument": map[string]any{
			"synchronization": map[string]any{
				"dynamicRegistration": false,
				"didOpen":             true,
				"didChange":           true,
				"didSave":             true,
			},
			"definition": map[string]any{
				"linkSupport": true,
			},
			"hover": map[string]any{
				"contentFormat": []string{"markdown", "plaintext"},
			},
		},
		"workspace": map[string]any{
			"workspaceFolders": true,
			"configuration":    true,
		},
	}
}
