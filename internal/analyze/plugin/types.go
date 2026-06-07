package plugin

// Plugin defines language-specific rules for file analysis.
type Plugin interface {
	// Name returns the language identifier (e.g., "go", "typescript").
	Name() string

	// Extensions returns file extensions for this language.
	Extensions() []string

	// IsTestFile checks if a filename matches test patterns.
	IsTestFile(filename string) bool

	// IsGenerated checks if file content indicates generated code.
	IsGenerated(head string) bool

	// ExtractImports extracts import statements from file lines.
	ExtractImports(lines []string) []string

	// ExtractSignatures extracts function/class signatures from file lines.
	ExtractSignatures(lines []string) []string

	// ExtractSampleChunks extracts representative code chunks for LLM context.
	ExtractSampleChunks(lines []string) []string
}

// Registry manages available language plugins.
type Registry struct {
	plugins map[string]Plugin
}

// NewRegistry creates an empty plugin registry.
func NewRegistry() *Registry {
	return &Registry{
		plugins: make(map[string]Plugin),
	}
}

// Register adds a plugin to the registry.
func (r *Registry) Register(p Plugin) {
	r.plugins[p.Name()] = p
}

// Get retrieves a plugin by language name.
func (r *Registry) Get(lang string) (Plugin, bool) {
	p, ok := r.plugins[lang]
	return p, ok
}

// GetByExtension finds a plugin that handles the given file extension.
func (r *Registry) GetByExtension(ext string) Plugin {
	for _, p := range r.plugins {
		for _, e := range p.Extensions() {
			if e == ext {
				return p
			}
		}
	}
	return nil
}

// All returns all registered plugins.
func (r *Registry) All() []Plugin {
	result := make([]Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		result = append(result, p)
	}
	return result
}
