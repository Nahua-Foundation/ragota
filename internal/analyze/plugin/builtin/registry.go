package builtin

import (
	"ragota/internal/analyze/plugin"
)

// DefaultRegistry returns a registry with all built-in plugins.
func DefaultRegistry() *plugin.Registry {
	reg := plugin.NewRegistry()

	// Register built-in plugins
	reg.Register(&GoPlugin{})
	reg.Register(&TypeScriptPlugin{})
	reg.Register(&PythonPlugin{})
	reg.Register(&JavaPlugin{})

	// Register generic plugins for other languages
	reg.Register(NewGenericPlugin("rust", []string{".rs"}))
	reg.Register(NewGenericPlugin("ruby", []string{".rb"}))
	reg.Register(NewGenericPlugin("php", []string{".php"}))
	reg.Register(NewGenericPlugin("csharp", []string{".cs"}))
	reg.Register(NewGenericPlugin("cpp", []string{".cpp", ".cc", ".cxx", ".h", ".hpp"}))
	reg.Register(NewGenericPlugin("c", []string{".c", ".h"}))
	reg.Register(NewGenericPlugin("kotlin", []string{".kt", ".kts"}))
	reg.Register(NewGenericPlugin("swift", []string{".swift"}))
	reg.Register(NewGenericPlugin("elixir", []string{".ex", ".exs"}))
	reg.Register(NewGenericPlugin("dart", []string{".dart"}))

	return reg
}
