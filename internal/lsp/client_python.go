package lsp

// pythonCapabilities возвращает capabilities для pyright.
func pythonCapabilities() map[string]any {
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
			"references": map[string]any{
				"dynamicRegistration": true,
			},
			"hover": map[string]any{
				"contentFormat": []string{"markdown", "plaintext"},
			},
			"documentSymbol": map[string]any{
				"hierarchicalDocumentSymbolSupport": true,
			},
			"symbol": map[string]any{
				"dynamicRegistration": true,
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

// pythonInitializationOptions возвращает initializationOptions для pyright.
func pythonInitializationOptions() map[string]any {
	return map[string]any{
		"analysis": map[string]any{
			"autoSearchPaths":        true,
			"useLibraryCodeForTypes": true,
			"diagnosticMode":         "openFilesOnly",
			"typeCheckingMode":       "basic",
			"autoImportCompletions":  true,
		},
	}
}

// pythonConfigFor возвращает настройки для pyright (workspace/configuration).
func pythonConfigFor(section string) any {
	pythonAnalysis := map[string]any{
		"autoSearchPaths":        true,
		"useLibraryCodeForTypes": true,
		"diagnosticMode":         "openFilesOnly",
		"typeCheckingMode":       "basic",
		"autoImportCompletions":  true,
	}
	pythonSettings := map[string]any{
		"pythonPath": "python3",
		"analysis":   pythonAnalysis,
	}
	pyrightSettings := map[string]any{
		"disableLanguageServices": false,
		"disableOrganizeImports":  false,
		"useLibraryCodeForTypes":  true,
	}
	switch section {
	case "python":
		return pythonSettings
	case "python.analysis":
		return pythonAnalysis
	case "pyright":
		return pyrightSettings
	case "":
		return map[string]any{
			"python":  pythonSettings,
			"pyright": pyrightSettings,
		}
	}
	return map[string]any{}
}
