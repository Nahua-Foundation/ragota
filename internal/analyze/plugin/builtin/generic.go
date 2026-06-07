package builtin

import (
	"strings"
)

// GenericPlugin is a fallback plugin for languages without specific support.
type GenericPlugin struct {
	name string
	exts []string
}

// NewGenericPlugin creates a generic plugin for a language.
func NewGenericPlugin(name string, extensions []string) *GenericPlugin {
	return &GenericPlugin{
		name: name,
		exts: extensions,
	}
}

func (p *GenericPlugin) Name() string { return p.name }

func (p *GenericPlugin) Extensions() []string {
	return p.exts
}

func (p *GenericPlugin) IsTestFile(filename string) bool {
	lower := strings.ToLower(filename)
	return strings.Contains(lower, "test") || strings.Contains(lower, "spec")
}

func (p *GenericPlugin) IsGenerated(head string) bool {
	lower := strings.ToLower(head)
	markers := []string{
		"generated",
		"auto-generated",
		"do not edit",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func (p *GenericPlugin) ExtractImports(lines []string) []string {
	// Generic: look for common import patterns
	var imports []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip comments
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, ";") {
			continue
		}

		// Common import keywords
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "import ") || strings.HasPrefix(lower, "include ") ||
			strings.HasPrefix(lower, "require ") || strings.HasPrefix(lower, "using ") {
			sig := trimmed
			if len(sig) > 120 {
				sig = sig[:120] + "..."
			}
			imports = append(imports, sig)
		}

		if len(imports) >= 15 {
			break
		}
	}
	return imports
}

func (p *GenericPlugin) ExtractSignatures(lines []string) []string {
	// Generic: look for function/class-like declarations
	var sigs []string
	keywords := []string{
		"function ", "func ", "def ", "fn ", "fun ",
		"class ", "interface ", "type ", "struct ",
		"public ", "private ", "protected ", "export ",
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip comments and empty lines
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "/*") {
			continue
		}

		lower := strings.ToLower(trimmed)
		for _, kw := range keywords {
			if strings.HasPrefix(lower, kw) {
				sig := trimmed
				if len(sig) > 100 {
					sig = sig[:100] + "..."
				}
				sigs = append(sigs, sig)
				break
			}
		}

		if len(sigs) >= 10 {
			break
		}
	}
	return sigs
}

func (p *GenericPlugin) ExtractSampleChunks(lines []string) []string {
	// Generic: find first non-comment, non-empty line with keywords
	keywords := []string{"function", "class", "def", "fn", "public", "private", "export"}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") {
			continue
		}

		lower := strings.ToLower(trimmed)
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				return []string{joinLines(lines, i, 5)}
			}
		}
	}

	return firstNonEmpty(lines, 5)
}
