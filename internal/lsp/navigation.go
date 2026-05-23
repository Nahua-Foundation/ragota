package lsp

import (
	"context"
	"encoding/json"
)

// Файл содержит публичные LSP-операции навигации поверх Call/Notify
// из jsonrpc.go: Definition, References, Hover, Implementation.

// Definition — go-to-definition. Если definition вернул пусто (например,
// для TS const/var или Java `new Foo()` когда курсор на скобке), пробуем
// declaration, typeDefinition, а затем повторяем запросы со сдвинутой
// позицией на ближайший идентификатор в строке.
func (c *Client) Definition(ctx context.Context, path string, line, character int) ([]Location, error) {
	params := c.positionParams(path, line, character)
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/definition", params, &raw); err != nil {
		lspDebug("LSP %s: Definition ERROR: %v\n", c.Language, err)
		return nil, err
	}
	locs := c.decodeLocations(raw)
	lspDebug("LSP %s: Definition RESULT: locations=%d raw=%q\n", c.Language, len(locs), string(raw))
	if len(locs) > 0 {
		return locs, nil
	}

	// Пробуем declaration и typeDefinition как фоллбэк
	for _, method := range []string{"textDocument/declaration", "textDocument/typeDefinition"} {
		var r json.RawMessage
		if err := c.Call(ctx, method, params, &r); err != nil {
			continue
		}
		if locs := c.decodeLocations(r); len(locs) > 0 {
			lspDebug("LSP %s: %s RESULT: locations=%d\n", c.Language, method, len(locs))
			return locs, nil
		}
	}

	// Эвристика: если мы на краю слова или внутри, пробуем найти начало идентификатора.
	// Часто клик/курсор попадает на скобку после имени или в конец слова, где LSP может не сработать.
	lineText := c.localFileLine(path, line)
	if lineText != "" && character > 0 {
		newChar := character
		// Если текущий символ не буква/цифра, пробуем шагнуть назад
		if character >= len(lineText) || !isAlphaNum(lineText[character]) {
			newChar--
		}
		// Ищем начало слова
		for newChar > 0 && isAlphaNum(lineText[newChar]) {
			newChar--
		}
		if newChar < character {
			if !isAlphaNum(lineText[newChar]) {
				newChar++
			}
			if newChar != character {
				lspDebug("LSP %s: Definition retry at shifted position: %d -> %d\n", c.Language, character, newChar)
				return c.Definition(ctx, path, line, newChar)
			}
		}
	}

	return nil, nil
}

// References — find references.
func (c *Client) References(ctx context.Context, path string, line, character int, includeDecl bool) ([]Location, error) {
	params := c.positionParams(path, line, character)
	params["context"] = map[string]any{"includeDeclaration": includeDecl}
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/references", params, &raw); err != nil {
		lspDebug("LSP %s: References ERROR: %v\n", c.Language, err)
		return nil, err
	}
	locs := c.decodeLocations(raw)
	lspDebug("LSP %s: References RESULT: locations=%d raw=%q\n", c.Language, len(locs), string(raw))
	return locs, nil
}

// Hover возвращает текст hover-подсказки (упрощённо — markdown/plain).
func (c *Client) Hover(ctx context.Context, path string, line, character int) (string, error) {
	params := c.positionParams(path, line, character)
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/hover", params, &raw); err != nil {
		lspDebug("LSP %s: Hover ERROR: %v\n", c.Language, err)
		return "", err
	}
	if len(raw) == 0 || string(raw) == "null" {
		lspDebug("LSP %s: Hover RESULT: empty\n", c.Language)
		return "", nil
	}
	var h struct {
		Contents any `json:"contents"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		lspDebug("LSP %s: Hover ERROR parse: %v\n", c.Language, err)
		return "", err
	}
	txt := hoverString(h.Contents)
	lspDebug("LSP %s: Hover RESULT: %d chars\n", c.Language, len(txt))
	return txt, nil
}

// Implementation — найти реализации интерфейса/метода.
func (c *Client) Implementation(ctx context.Context, path string, line, character int) ([]Location, error) {
	params := c.positionParams(path, line, character)
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/implementation", params, &raw); err != nil {
		return nil, err
	}
	return c.decodeLocations(raw), nil
}
