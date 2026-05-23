package lsp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Файл содержит публичный тип Location, внутренние представления LSP-объектов
// (Range/Position/Location/LocationLink/DocumentSymbol), а также декодеры
// результатов textDocument/* запросов в наш Location и хелпер positionParams.
//
// Эти типы и хелперы используются как jsonrpc-слоем (для разбора результатов
// серверных запросов вроде workspace/configuration), так и navigation.go.

// Location — упрощённое представление LSP Location, как мы отдаём его наружу.
type Location struct {
	URI       string `json:"uri"`
	StartLine int    `json:"start_line"`
	StartChar int    `json:"start_char"`
	EndLine   int    `json:"end_line"`
	EndChar   int    `json:"end_char"`
}

// lspRange представляет диапазон в документе.
type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

// lspLocation как приходит от сервера.
type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

// lspLocationLink (LocationLink) как приходит от сервера для Definition.
type lspLocationLink struct {
	TargetURI            string   `json:"targetUri"`
	TargetRange          lspRange `json:"targetRange"`
	TargetSelectionRange lspRange `json:"targetSelectionRange"`
}

// lspPosition — точка в документе (line/character, 0-based).
type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// lspDocumentSymbol — иерархический символ из textDocument/documentSymbol.
type lspDocumentSymbol struct {
	Name           string              `json:"name"`
	Kind           int                 `json:"kind"`
	Range          lspRange            `json:"range"`
	SelectionRange lspRange            `json:"selectionRange"`
	Children       []lspDocumentSymbol `json:"children"`
}

// locFromLSP конвертирует массив серверных Location в наш формат с локальным URI.
func (c *Client) locFromLSP(in []lspLocation) []Location {
	out := make([]Location, 0, len(in))
	for _, l := range in {
		out = append(out, Location{
			URI:       FileURI(c.toLocalPath(l.URI)),
			StartLine: l.Range.Start.Line, StartChar: l.Range.Start.Character,
			EndLine: l.Range.End.Line, EndChar: l.Range.End.Character,
		})
	}
	return out
}

// locFromLinks конвертирует массив LocationLink (используется gopls в Definition)
// в наш формат.
func (c *Client) locFromLinks(in []lspLocationLink) []Location {
	out := make([]Location, 0, len(in))
	for _, l := range in {
		out = append(out, Location{
			URI:       FileURI(c.toLocalPath(l.TargetURI)),
			StartLine: l.TargetSelectionRange.Start.Line, StartChar: l.TargetSelectionRange.Start.Character,
			EndLine: l.TargetSelectionRange.End.Line, EndChar: l.TargetSelectionRange.End.Character,
		})
	}
	return out
}

// decodeLocations пытается распарсить «полиморфный» результат LSP:
// Location | Location[] | LocationLink | LocationLink[].
func (c *Client) decodeLocations(raw json.RawMessage) []Location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var single lspLocation
	if err := json.Unmarshal(raw, &single); err == nil && single.URI != "" {
		return c.locFromLSP([]lspLocation{single})
	}
	var arr []lspLocation
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 && arr[0].URI != "" {
		return c.locFromLSP(arr)
	}
	var singleLink lspLocationLink
	if err := json.Unmarshal(raw, &singleLink); err == nil && singleLink.TargetURI != "" {
		return c.locFromLinks([]lspLocationLink{singleLink})
	}
	var links []lspLocationLink
	if err := json.Unmarshal(raw, &links); err == nil && len(links) > 0 && links[0].TargetURI != "" {
		return c.locFromLinks(links)
	}
	return nil
}

// decodeLocationsWithErr возвращает ошибку если LSP сервер вернул явную ошибку
// или пустой результат с известным сообщением (например "no views").
func (c *Client) decodeLocationsWithErr(raw json.RawMessage) ([]Location, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// Проверяем, не вернул ли сервер ошибку в виде map с message.
	var errMap map[string]any
	if err := json.Unmarshal(raw, &errMap); err == nil {
		if msg, ok := errMap["message"].(string); ok {
			return nil, fmt.Errorf("LSP error: %s", msg)
		}
	}
	if locs := c.decodeLocations(raw); len(locs) > 0 {
		return locs, nil
	}
	return nil, nil
}

// positionParams создаёт параметры для запроса на позицию (textDocument+position).
func (c *Client) positionParams(path string, line, character int) map[string]any {
	remote := c.toRemotePath(path)
	uri := FileURI(remote)
	return map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
	}
}

// hoverString преобразует hover contents (string | MarkupContent | array of those)
// в плоскую строку, пригодную для вывода пользователю.
func hoverString(contents any) string {
	if contents == nil {
		return ""
	}
	switch v := contents.(type) {
	case string:
		return v
	case map[string]any:
		if val, ok := v["value"].(string); ok {
			return val
		}
		if val, ok := v["kind"].(string); ok && val == "plaintext" {
			if vval, ok := v["value"].(string); ok {
				return vval
			}
		}
		if val, ok := v["kind"].(string); ok && val == "markdown" {
			if vval, ok := v["value"].(string); ok {
				return vval
			}
		}
	case []any:
		var parts []string
		for _, item := range v {
			if s := hoverString(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	data, _ := json.Marshal(contents)
	return string(data)
}
