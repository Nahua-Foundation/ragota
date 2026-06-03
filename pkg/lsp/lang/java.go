package lang

// JavaCapabilities returns capabilities for jdtls.
func JavaCapabilities() map[string]any {
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
			"documentSymbol": map[string]any{
				"hierarchicalDocumentSymbolSupport": true,
			},
		},
		"workspace": map[string]any{
			"workspaceFolders": true,
			"configuration":    true,
			"workspaceEdit": map[string]any{
				"documentChanges": true,
			},
		},
	}
}

// JavaInitOptions returns initializationOptions for jdtls.
func JavaInitOptions() map[string]any {
	return map[string]any{
		"bundles":         []any{},
		"extendedClientCapabilities": map[string]any{
			"classFileContentsSupport": true,
		},
	}
}

// JavaConfigFor returns settings for jdtls (workspace/configuration).
func JavaConfigFor(section string) any {
	javaSettings := map[string]any{
		"home":                    "",
		"jdt.ls.java.home":        "",
		"completion.enabled":      true,
		"completion.overwrite":    true,
		"format.enabled":          true,
		"format.onType.enabled":   true,
		"maven.downloadSources":   true,
		"implementationsCodeLens.enabled": true,
		"referencesCodeLens.enabled":      true,
		"signatureHelp.enabled":           true,
		"sources.organizeImports.enabled": true,
	}
	switch section {
	case "java", "":
		return javaSettings
	}
	return map[string]any{}
}
