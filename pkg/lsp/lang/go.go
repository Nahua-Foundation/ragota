package lang

// GoCapabilities returns capabilities for gopls.
func GoCapabilities() map[string]any {
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
			"implementation": map[string]any{
				"linkSupport": true,
			},
			"hover": map[string]any{
				"contentFormat": []string{"markdown", "plaintext"},
			},
			"completion": map[string]any{
				"completionItem": map[string]any{
					"snippetSupport":          true,
					"commitCharactersSupport": true,
				},
			},
		},
		"workspace": map[string]any{
			"workspaceFolders": true,
			"configuration":    true,
			"workspaceEdit": map[string]any{
				"documentChanges": true,
			},
		},
		"window": map[string]any{
			"workDoneProgress": true,
		},
	}
}

// GoInitOptions returns initializationOptions for gopls.
func GoInitOptions() map[string]any {
	return map[string]any{
		"ui.semanticTokens":             true,
		"ui.completion.usePlaceholders": true,
		"ui.diagnostic.analyses": map[string]any{
			"unusedparams": true,
			"shadow":       true,
		},
		"ui.diagnostic.staticcheck":    true,
		"build.allowImplicitNetworks":  true,
		"build.directoryFilters":       []string{"-vendor", "-node_modules", "-.git"},
		"ui.navigation.importShortcut": "Both",
		"hoverKind":                    "Full",
	}
}

// GoConfigFor returns settings for gopls (workspace/configuration).
func GoConfigFor(section string) any {
	settings := map[string]any{
		"gopls": map[string]any{
			"ui.semanticTokens":             true,
			"ui.completion.usePlaceholders": true,
			"ui.diagnostic.analyses": map[string]any{
				"unusedparams": true,
				"shadow":       true,
			},
			"ui.diagnostic.staticcheck":    true,
			"build.allowImplicitNetworks":  true,
			"formatting.gofumpt":           false,
			"ui.navigation.importShortcut": "Both",
			"hoverKind":                    "Full",
		},
	}
	switch section {
	case "gopls", "":
		return settings["gopls"]
	}
	return map[string]any{}
}
