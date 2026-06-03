package lang

// TypeScriptCapabilities returns capabilities for typescript-language-server.
func TypeScriptCapabilities() map[string]any {
	return map[string]any{
		"textDocument": map[string]any{
			"synchronization": map[string]any{
				"dynamicRegistration": false,
				"didOpen":             true,
				"didChange":           true,
				"didSave":             true,
				"willSave":            false,
				"willSaveWaitUntil":   false,
			},
			"definition": map[string]any{
				"linkSupport": true,
			},
			"declaration": map[string]any{
				"linkSupport": true,
			},
			"typeDefinition": map[string]any{
				"linkSupport": true,
			},
			"implementation": map[string]any{
				"linkSupport": true,
			},
			"references": map[string]any{
				"dynamicRegistration": true,
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

// TypeScriptInitOptions returns initializationOptions for typescript-language-server.
func TypeScriptInitOptions() map[string]any {
	return map[string]any{
		"preferences": map[string]any{
			"includeCompletionsForModuleExports": true,
			"includeCompletionsWithInsertText":   true,
		},
	}
}

// TypeScriptConfigFor returns settings for typescript-language-server (workspace/configuration).
func TypeScriptConfigFor(section string) any {
	switch section {
	case "typescript", "javascript":
		return map[string]any{
			"preferences": map[string]any{
				"includeCompletionsForModuleExports": true,
				"includeCompletionsWithInsertText":   true,
			},
		}
	default:
		return map[string]any{}
	}
}
