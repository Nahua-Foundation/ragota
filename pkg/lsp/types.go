package lsp

import (
	"encoding/json"
	"strings"
)

// Location — simplified LSP Location as exposed to callers.
type Location struct {
	URI       string `json:"uri"`
	StartLine int    `json:"start_line"`
	StartChar int    `json:"start_char"`
	EndLine   int    `json:"end_line"`
	EndChar   int    `json:"end_char"`
}

// lspRange represents a range in a document.
type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

// lspPosition represents a position in a document.
type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// lspLocation as received from the server.
type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

// lspLocationLink as received from the server (for definition with linkSupport).
type lspLocationLink struct {
	TargetURI          string   `json:"targetUri"`
	TargetRange        lspRange `json:"targetRange"`
	TargetSelectionRange lspRange `json:"targetSelectionRange"`
}

// lspDocumentSymbol as received from the server (hierarchical).
type lspDocumentSymbol struct {
	Name           string              `json:"name"`
	Kind           int                 `json:"kind"`
	Range          lspRange            `json:"range"`
	SelectionRange lspRange            `json:"selectionRange"`
	Children       []lspDocumentSymbol `json:"children"`
}

// DecodeLocations decodes raw JSON result from textDocument/* into []Location.
// Handles both Location[] and LocationLink[] responses.
// remoteRoot/hostRoot are used to convert server URIs to local paths.
func DecodeLocations(raw json.RawMessage, remoteRoot, hostRoot string) []Location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// Try Location[]
	var locs []lspLocation
	if err := json.Unmarshal(raw, &locs); err == nil && len(locs) > 0 && locs[0].URI != "" {
		out := make([]Location, 0, len(locs))
		for _, l := range locs {
			localPath := ToLocalPath(l.URI, remoteRoot, hostRoot)
			out = append(out, Location{
				URI:       localPath,
				StartLine: l.Range.Start.Line,
				StartChar: l.Range.Start.Character,
				EndLine:   l.Range.End.Line,
				EndChar:   l.Range.End.Character,
			})
		}
		return out
	}
	// Try LocationLink[]
	var links []lspLocationLink
	if err := json.Unmarshal(raw, &links); err == nil && len(links) > 0 {
		out := make([]Location, 0, len(links))
		for _, l := range links {
			localPath := ToLocalPath(l.TargetURI, remoteRoot, hostRoot)
			r := l.TargetSelectionRange
			if r.End.Line == 0 && r.End.Character == 0 {
				r = l.TargetRange
			}
			out = append(out, Location{
				URI:       localPath,
				StartLine: r.Start.Line,
				StartChar: r.Start.Character,
				EndLine:   r.End.Line,
				EndChar:   r.End.Character,
			})
		}
		return out
	}
	// Try single Location
	var single lspLocation
	if err := json.Unmarshal(raw, &single); err == nil && single.URI != "" {
		localPath := ToLocalPath(single.URI, remoteRoot, hostRoot)
		return []Location{{
			URI:       localPath,
			StartLine: single.Range.Start.Line,
			StartChar: single.Range.Start.Character,
			EndLine:   single.Range.End.Line,
			EndChar:   single.Range.End.Character,
		}}
	}
	return nil
}

// HoverString extracts text from hover contents (supports MarkupContent, MarkedString, array).
func HoverString(contents any) string {
	switch v := contents.(type) {
	case string:
		return v
	case map[string]any:
		if value, ok := v["value"].(string); ok {
			if lang, ok := v["language"].(string); ok && lang != "" {
				return "```" + lang + "\n" + value + "\n```"
			}
			return value
		}
		if kind, ok := v["kind"].(string); ok {
			if value, ok := v["value"].(string); ok {
				if kind == "markdown" {
					return value
				}
				return value
			}
		}
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if s := HoverString(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}
