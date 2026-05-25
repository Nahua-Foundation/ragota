package lsp

// typescriptCapabilities возвращает capabilities для tsserver.
func typescriptCapabilities() map[string]any {
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
			"implementation": map[string]any{
				"linkSupport": false,
			},
			"typeDefinition": map[string]any{
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
			"workspaceFolders": map[string]any{
				"supported":           true,
				"changeNotifications": true,
			},
			"configuration": true,
			"didChangeWatchedFiles": map[string]any{
				"dynamicRegistration": true,
			},
		},
	}
}

// typescriptInitializationOptions возвращает initializationOptions для tsserver.
func typescriptInitializationOptions() map[string]any {
	return nil
}

// typescriptConfigFor возвращает настройки для tsserver (workspace/configuration).
func typescriptConfigFor(section string) any {
	tsSettings := map[string]any{
		"format":      map[string]any{"enable": true},
		"suggest":     map[string]any{"completeFunctionCalls": true},
		"preferences": map[string]any{"importModuleSpecifier": "shortest"},
	}
	jsSettings := map[string]any{
		"format":  map[string]any{"enable": true},
		"suggest": map[string]any{"completeFunctionCalls": true},
	}
	switch section {
	case "typescript":
		return tsSettings
	case "javascript":
		return jsSettings
	case "completions":
		return map[string]any{"completeFunctionCalls": true}
	case "":
		return map[string]any{
			"typescript":  tsSettings,
			"javascript":  jsSettings,
			"completions": map[string]any{"completeFunctionCalls": true},
		}
	}
	return map[string]any{}
}
