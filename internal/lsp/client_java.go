package lsp

// javaCapabilities возвращает capabilities для jdtls.
func javaCapabilities() map[string]any {
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
			"implementation": map[string]any{
				"linkSupport": true,
			},
			"hover": map[string]any{
				"contentFormat": []string{"markdown", "plaintext"},
			},
			"completion": map[string]any{
				"completionItem": map[string]any{
					"snippetSupport": true,
				},
			},
			"documentSymbol": map[string]any{
				"hierarchicalDocumentSymbolSupport": true,
			},
			"symbol": map[string]any{
				"dynamicRegistration": true,
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

// javaInitializationOptions возвращает initializationOptions для jdtls.
func javaInitializationOptions() map[string]any {
	return map[string]any{
		"bundles": []any{},
		"settings": map[string]any{
			"java": javaConfigFor("java"),
		},
		"workspaceFolders": true,
	}
}

// javaConfigFor возвращает настройки для jdtls (workspace/configuration).
func javaConfigFor(section string) any {
	settings := map[string]any{
		"java.completion.enabled":              true,
		"java.completion.guessMethodArguments": true,
		"java.completion.importOrder":          "java,javax,org,com",
		"java.saveActions.organizeImports":     true,
		"java.server.launchMode":               "Standard",
		"java.configuration.updateBuildPath":   true,
		"java.import.gradle.enabled":           true,
		"java.import.maven.enabled":            true,
	}
	switch section {
	case "java", "":
		return settings
	}
	return map[string]any{}
}
