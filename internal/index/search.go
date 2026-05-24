package index

// Файл содержит публичный API поиска (Search, SimilarToUnit) и общие
// helper-функции пакета (combinedText, chunkID, buildFilter).

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"os"

	"aitools/internal/chunker"
	"aitools/internal/config"
	"aitools/internal/embedder"
	"aitools/internal/qdrant"
	"aitools/internal/store"
)

// Search — семантический поиск top-K. Если в filter указан language —
// автоматически выбирается соответствующая коллекция; иначе ищет в обеих
// и объединяет результаты, отсортировав по score.
func (v *Vector) Search(ctx context.Context, query string, limit int, filter map[string]any) ([]qdrant.SearchHit, error) {
	if limit <= 0 {
		limit = 10
	}
	lang, _ := filter["language"].(string)

	var collections []config.CollectionSpec
	var embs []*embedder.Ollama
	if lang != "" {
		sp, e := v.pickCollection(lang)
		collections = []config.CollectionSpec{sp}
		embs = []*embedder.Ollama{e}
	} else {
		collections = []config.CollectionSpec{v.cfg.CodeCollection(), v.cfg.TextCollection()}
		embs = []*embedder.Ollama{v.code, v.text}
	}

	f := buildFilter(filter)

	var all []qdrant.SearchHit
	for i, sp := range collections {
		vec, err := embs[i].Embed(ctx, query)
		if err != nil {
			// Embed может фейлиться, если модель не загружена; пропускаем
			// коллекцию с warning'ом, но не валим весь поиск.
			log.Printf("vector: embed for %q failed: %v", sp.Name, err)
			continue
		}
		hits, err := v.qd.Search(ctx, sp.Name, vec, limit, f)
		if err != nil {
			log.Printf("vector: qdrant search %q: %v", sp.Name, err)
			continue
		}
		all = append(all, hits...)
	}

	// Сортируем по score (Qdrant отдаёт по убыванию для cosine — это уже так).
	// Стабильно объединяем и обрезаем.
	if len(all) > limit {
		// Простое top-K merge: отсортируем убывая по score.
		for i := 0; i < len(all); i++ {
			for j := i + 1; j < len(all); j++ {
				if all[j].Score > all[i].Score {
					all[i], all[j] = all[j], all[i]
				}
			}
		}
		all = all[:limit]
	}
	return all, nil
}

// buildFilter преобразует «плоский» map фильтров в Qdrant-формат.
//
// Поддерживается специальный ключ "repo" со значениями:
//   - string ""  или "*"        — фильтр не применяется (все репо);
//   - string "name"             — точное совпадение repo == name;
//   - []string{...}             — OR по нескольким репам (Qdrant `should`);
//   - []any{...}                — то же, что []string (после приведения).
//
// Остальные ключи трактуются как простые match-фильтры (AND через `must`).
func buildFilter(filter map[string]any) map[string]any {
	if len(filter) == 0 {
		return nil
	}
	must := make([]map[string]any, 0, len(filter))
	for k, val := range filter {
		if k == "repo" {
			if cond := repoMatchCondition(val); cond != nil {
				must = append(must, cond)
			}
			continue
		}
		must = append(must, map[string]any{"key": k, "match": map[string]any{"value": val}})
	}
	if len(must) == 0 {
		return nil
	}
	return map[string]any{"must": must}
}

// repoMatchCondition строит Qdrant-условие фильтрации по полю `repo`.
// Возвращает nil, если фильтр не нужен ("*" или пустое значение).
func repoMatchCondition(val any) map[string]any {
	switch v := val.(type) {
	case string:
		if v == "" || v == "*" {
			return nil
		}
		return map[string]any{"key": "repo", "match": map[string]any{"value": v}}
	case []string:
		names := make([]string, 0, len(v))
		for _, s := range v {
			if s == "" || s == "*" {
				return nil
			}
			names = append(names, s)
		}
		if len(names) == 0 {
			return nil
		}
		if len(names) == 1 {
			return map[string]any{"key": "repo", "match": map[string]any{"value": names[0]}}
		}
		return map[string]any{"key": "repo", "match": map[string]any{"any": names}}
	case []any:
		names := make([]string, 0, len(v))
		for _, x := range v {
			s, _ := x.(string)
			if s == "" || s == "*" {
				return nil
			}
			names = append(names, s)
		}
		if len(names) == 0 {
			return nil
		}
		if len(names) == 1 {
			return map[string]any{"key": "repo", "match": map[string]any{"value": names[0]}}
		}
		return map[string]any{"key": "repo", "match": map[string]any{"any": names}}
	}
	return nil
}

// SimilarToUnit реализует symbols.SimilarSearcher: ищет AST units, чьи
// эмбеддинги ближе всего к содержимому unit'а u. Сейчас — приближение
// через text-based семантический поиск по подписи + первой строке тела.
func (v *Vector) SimilarToUnit(ctx context.Context, u store.ASTUnit, limit int) ([]store.ASTUnit, error) {
	if v.store == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	// Читаем подпись + первые строки тела как query.
	query := u.Signature
	if query == "" {
		query = u.Name
	}
	if u.FilePath != "" && u.StartByte < u.EndByte {
		if src, err := os.ReadFile(u.FilePath); err == nil {
			start := u.StartByte
			end := u.EndByte
			if end > len(src) {
				end = len(src)
			}
			if start < end {
				snippet := string(src[start:end])
				if len(snippet) > 1500 {
					snippet = snippet[:1500]
				}
				query = snippet
			}
		}
	}
	hits, err := v.Search(ctx, query, limit*2, map[string]any{"language": u.Language})
	if err != nil {
		return nil, err
	}
	out := make([]store.ASTUnit, 0, limit)
	seen := map[int64]bool{u.ID: true}
	for _, h := range hits {
		path, _ := h.Payload["file"].(string)
		if path == "" {
			continue
		}
		units, err := v.store.ListASTUnitsByFile(ctx, path)
		if err != nil {
			continue
		}
		// Берём unit, чья область [start_line, end_line] пересекается с
		// чанком hit'а.
		startLine, _ := h.Payload["start_line"].(float64)
		endLine, _ := h.Payload["end_line"].(float64)
		var best *store.ASTUnit
		for i := range units {
			cu := units[i]
			if cu.Kind == "module" || seen[cu.ID] {
				continue
			}
			// Берём unit, чей диапазон строк пересекается с чанком.
			sl := int(startLine)
			el := int(endLine)
			if sl <= cu.EndLine && el >= cu.StartLine {
				// Если чанк пересекается с юнитом, считаем это попаданием.
				// Выбираем наиболее специфичный (короткий) юнит, если их несколько.
				if best == nil || (cu.EndLine-cu.StartLine) < (best.EndLine-best.StartLine) {
					best = &cu
				}
			}
		}
		if best != nil {
			seen[best.ID] = true
			out = append(out, *best)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// combinedText формирует текст для эмбеддинга: лидирующие doc-комментарии
// (если есть) + тело чанка. Это улучшает семантический поиск, так как
// комментарии часто содержат намерение/описание кода на естественном языке.
func combinedText(ch chunker.Chunk) string {
	if ch.Comments == "" {
		return ch.Text
	}
	return ch.Comments + "\n" + ch.Text
}

// chunkID — детерминированный hex-id (sha1[:32]).
func chunkID(file string, idx int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s#%d", file, idx)))
	hexStr := hex.EncodeToString(h[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
}
