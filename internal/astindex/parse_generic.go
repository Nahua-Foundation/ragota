package astindex

// Файл содержит generic-экстрактор для не-Go языков (Python и др.):
// делегирует tree-sitter (internal/parser.Parser) и превращает его
// symbols в AST units. Edges не извлекаются — это согласовано со
// стратегией «Go-first» (см. документацию пакета).

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"

	"aitools/internal/store"
)

// parseGeneric делегирует tree-sitter (через internal/parser.Parser) и
// превращает его symbols в AST units. Edges не извлекаются.
func (i *Indexer) parseGeneric(ctx context.Context, lang, path string, src []byte) ([]store.ASTUnit, error) {
	if lang == "" {
		return nil, nil
	}
	syms, err := i.ts.Parse(ctx, lang, path, src)
	if err != nil {
		return nil, err
	}

	moduleName := filepath.Base(path)
	units := []store.ASTUnit{{
		FilePath:  path,
		Language:  lang,
		Kind:      "module",
		Name:      moduleName,
		Qualified: moduleName,
		StartLine: 1,
		EndLine:   1 + strings.Count(string(src), "\n"),
		StartByte: 0,
		EndByte:   len(src),
		Hash:      hashBytes(src),
	}}

	// parent_id всех unit'ов в этом файле = module (idx 0). Класс/интерфейс
	// тоже привязываем к module, а методы — к соответствующему классу
	// если он встретился (упрощённая 1-level вложенность).
	classIdx := map[string]int{}
	for _, sym := range syms {
		if sym.Name == "" {
			continue
		}
		parent := 0
		qualified := moduleName + "." + sym.Name
		if sym.Parent != "" {
			if ci, ok := classIdx[sym.Parent]; ok {
				parent = ci
			}
			qualified = moduleName + "." + sym.Parent + "." + sym.Name
		}
		body := ""
		if sym.StartByte >= 0 && sym.EndByte <= len(src) && sym.StartByte < sym.EndByte {
			body = string(src[sym.StartByte:sym.EndByte])
		}
		idx := len(units)
		units = append(units, store.ASTUnit{
			FilePath:  path,
			Language:  lang,
			Kind:      sym.Kind,
			Name:      sym.Name,
			Qualified: qualified,
			ParentID:  sql.NullInt64{Int64: int64(parent), Valid: true},
			StartLine: sym.StartLine,
			EndLine:   sym.EndLine,
			StartByte: sym.StartByte,
			EndByte:   sym.EndByte,
			Signature: sym.Signature,
			Hash:      hashBytes([]byte(body)),
		})
		if sym.Kind == "class" || sym.Kind == "interface" {
			classIdx[sym.Name] = idx
		}
	}
	return units, nil
}
