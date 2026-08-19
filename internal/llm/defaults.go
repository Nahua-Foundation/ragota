package llm

// Default provider endpoints. The constructors fall back to these when the
// config names no base_url; they are exported so --check-config probes the
// same endpoint the server will actually dial — two copies of a default is
// how a wiring check ends up vouching for the wrong address.
const (
	// DefaultOllamaBaseURL is where a locally installed Ollama listens.
	DefaultOllamaBaseURL = "http://localhost:11434"
	// DefaultOpenAIBaseURL is the public OpenAI endpoint, without the "/v1"
	// segment (see normalizeOpenAIBase).
	DefaultOpenAIBaseURL = "https://api.openai.com"
)

// DefaultBaseURL returns the built-in endpoint for a provider, or "" for a
// provider that has no default and must be configured explicitly.
func DefaultBaseURL(provider string) string {
	switch provider {
	case "ollama":
		return DefaultOllamaBaseURL
	case "openai":
		return DefaultOpenAIBaseURL
	}
	return ""
}
