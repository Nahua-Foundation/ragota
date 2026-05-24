package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"aitools/internal/store"

	"github.com/mark3labs/mcp-go/mcp"
)

// parseRepoParam парсит MCP-параметр `repo` и возвращает значение,
// пригодное для index.buildFilter:
//
//   - "" или "*"            — nil (фильтр не применяется);
//   - "name"                — строка "name";
//   - JSON-массив "[\"a\",\"b\"]" — []string{"a","b"};
//   - CSV "a,b"             — []string{"a","b"}.
//
// Если строка не парсится как JSON-массив, она трактуется как одиночное
// имя репы (или CSV, если содержит запятые).
func parseRepoParam(raw string) any {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			return normalizeRepoList(arr)
		}
		// Невалидный JSON — fallback на строку как есть.
	}
	if strings.Contains(raw, ",") {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return normalizeRepoList(out)
	}
	return raw
}

// normalizeRepoList — если список содержит "*", фильтр отменяется (nil);
// если в списке одно имя — возвращаем строку; иначе — []string.
func normalizeRepoList(list []string) any {
	if len(list) == 0 {
		return nil
	}
	for _, s := range list {
		if s == "*" {
			return nil
		}
	}
	if len(list) == 1 {
		return list[0]
	}
	return list
}

// parseRepoListParam — аналог parseRepoParam, но всегда возвращает
// []string (или nil). Используется для bm25.Query.Repos.
func parseRepoListParam(raw string) []string {
	v := parseRepoParam(raw)
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return []string{x}
	case []string:
		return x
	}
	return nil
}

// jsonResult — упаковывает payload в текстовый MCP-ответ (JSON).
// Никогда не возвращает null для nil слайсов, так как они должны быть
// инициализированы в store. Но для надежности проверяет.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	if v == nil {
		return mcp.NewToolResultText("[]"), nil
	}
	// Если это слайс и он nil, вернем "[]"
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		return mcp.NewToolResultText("[]"), nil
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(data)), nil
}

// repoMatcher строит предикат для фильтрации по имени репы. Возвращает
// (predicate, ok): если ok=false — фильтр не применять (пусто/"*").
// Поддерживает значения, выдаваемые parseRepoParam: nil, string, []string.
func repoMatcher(v any) (func(string) bool, bool) {
	switch x := v.(type) {
	case nil:
		return nil, false
	case string:
		if x == "" || x == "*" {
			return nil, false
		}
		return func(r string) bool { return r == x }, true
	case []string:
		if len(x) == 0 {
			return nil, false
		}
		set := make(map[string]struct{}, len(x))
		for _, s := range x {
			if s == "*" {
				return nil, false
			}
			set[s] = struct{}{}
		}
		return func(r string) bool { _, ok := set[r]; return ok }, true
	}
	return nil, false
}

// filterUnitsByRepo возвращает копию units, оставляя только те, чей Repo
// совпадает с фильтром. Если фильтр пуст — возвращает units как есть.
func filterUnitsByRepo(units []store.ASTUnit, repoFilter any) []store.ASTUnit {
	match, ok := repoMatcher(repoFilter)
	if !ok {
		return units
	}
	out := make([]store.ASTUnit, 0, len(units))
	for _, u := range units {
		if match(u.Repo) {
			out = append(out, u)
		}
	}
	return out
}

// filterEdgesByRepo — аналог для рёбер.
func filterEdgesByRepo(edges []store.Edge, repoFilter any) []store.Edge {
	match, ok := repoMatcher(repoFilter)
	if !ok {
		return edges
	}
	out := make([]store.Edge, 0, len(edges))
	for _, e := range edges {
		if match(e.Repo) {
			out = append(out, e)
		}
	}
	return out
}

// errorToResult превращает ошибку в успешный ToolResult с пометкой Error.
// Это позволяет LLM увидеть текст ошибки и попробовать другой путь.
func errorToResult(name string, err error) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(fmt.Sprintf("Tool %s failed: %v", name, err)), nil
}
