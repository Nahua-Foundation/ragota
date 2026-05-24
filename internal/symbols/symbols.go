// Package symbols — symbol-aware retrieval поверх AST units.
//
// Сервис объединяет:
//   - извлечение AST units из tree-sitter (через internal/parser)
//   - построение рёбер графа (calls/imports/implementations/...)
//   - симметричный API для MCP: find_definition / find_references /
//     find_implementations / find_callers / find_callees /
//     get_file_symbols / get_symbol / get_parent / get_children /
//     get_surrounding_context / get_related_files / get_similar_code
package symbols

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"

	"aitools/internal/graph"
	"aitools/internal/store"
)

// ErrNotFound — символ не найден.
var ErrNotFound = errors.New("symbols: not found")

// SimilarSearcher — необязательный поставщик «похожего кода» (через
// векторный индекс). Если не передан — get_similar_code вернёт пустой
// список без ошибки.
type SimilarSearcher interface {
	SimilarToUnit(ctx context.Context, u store.ASTUnit, limit int) ([]store.ASTUnit, error)
}

// Service — высокоуровневый сервис символов и AST units.
type Service struct {
	st  *store.SQLite
	g   *graph.Service
	sim SimilarSearcher
}

// New создаёт сервис. sim может быть nil.
func New(st *store.SQLite, g *graph.Service, sim SimilarSearcher) *Service {
	return &Service{st: st, g: g, sim: sim}
}

// SetSimilarSearcher позднее подключает векторный поиск (после создания).
func (s *Service) SetSimilarSearcher(sim SimilarSearcher) { s.sim = sim }

// ----- AST / structure retrieval -----

func (s *Service) FileSymbols(ctx context.Context, path string) ([]store.ASTUnit, error) {
	return s.st.ListASTUnitsByFile(ctx, path)
}

func (s *Service) Get(ctx context.Context, id int64) (*store.ASTUnit, error) {
	return s.st.GetASTUnit(ctx, id)
}

func (s *Service) Parent(ctx context.Context, id int64) (*store.ASTUnit, error) {
	u, err := s.st.GetASTUnit(ctx, id)
	if err != nil || u == nil {
		return nil, err
	}
	if !u.ParentID.Valid {
		return nil, nil
	}
	return s.st.GetASTUnit(ctx, u.ParentID.Int64)
}

func (s *Service) Children(ctx context.Context, id int64) ([]store.ASTUnit, error) {
	return s.st.ChildrenOf(ctx, id)
}

// ----- Symbol-aware lookup -----

func (s *Service) FindDefinition(ctx context.Context, symbol string) ([]store.ASTUnit, error) {
	// Ищем определения (любой kind, кроме module — модуль не «определение символа»).
	units, err := s.st.FindASTUnits(ctx, symbol, "", "", 20)
	if err != nil {
		return nil, err
	}
	out := []store.ASTUnit{}
	for _, u := range units {
		if u.Kind == "module" {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

func (s *Service) FindReferences(ctx context.Context, symbol string) ([]store.Edge, error) {
	// 1. Найти все определения с этим именем (может быть pkg.Func или просто Func).
	defs, err := s.FindDefinition(ctx, symbol)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []store.Edge = []store.Edge{}

	// 2. Для каждого определения ищем разрешённые ссылки (dst_id).
	// Также собираем (имя, язык) для языко-чувствительного поиска
	// нерезолвленных рёбер — иначе TS-`log()` подтянет Go-`log`.
	type nameLangKey struct {
		name string
		lang string
	}
	namesToSearch := map[nameLangKey]bool{}
	for _, d := range defs {
		es, err := s.g.References(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			out = append(out, e)
		}
		namesToSearch[nameLangKey{d.Name, d.Language}] = true
		if d.Qualified != "" {
			namesToSearch[nameLangKey{d.Qualified, d.Language}] = true
		}
	}

	// 3. Дополнительно — edges по именам (для внешних/нерезолвленных) с
	// фильтром по языку определения. Если defs пуст (например, символ
	// внешний), деградируем до прежнего поведения с предупреждением о
	// кросс-языковом матче — но только под исходным symbol без языка.
	if len(namesToSearch) == 0 {
		byName, err := s.st.EdgesByDstName(ctx, symbol, graph.EdgeReference)
		if err != nil {
			return nil, err
		}
		for _, e := range byName {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			out = append(out, e)
		}
		return out, nil
	}

	for k := range namesToSearch {
		byName, err := s.st.EdgesByDstNameForLang(ctx, k.name, graph.EdgeReference, k.lang)
		if err != nil {
			return nil, err
		}
		for _, e := range byName {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *Service) FindImplementations(ctx context.Context, iface string) ([]store.ASTUnit, error) {
	units, err := s.st.FindASTUnits(ctx, iface, "interface", "", 5)
	if err != nil {
		return nil, err
	}
	out := []store.ASTUnit{}
	seen := map[int64]bool{}

	// 1. По ID найденных интерфейсов
	for _, u := range units {
		impls, err := s.g.Implementations(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		for _, im := range impls {
			if seen[im.ID] {
				continue
			}
			seen[im.ID] = true
			out = append(out, im)
		}

		// Эвристика для Go: если интерфейс имеет методы, ищем структуры с такими же методами
		if u.Language == "go" {
			methods, err := s.st.ChildrenOf(ctx, u.ID)
			if err == nil && len(methods) > 0 {
				methodNames := make([]string, 0, len(methods))
				for _, m := range methods {
					if m.Kind == "method" || m.Kind == "function" {
						methodNames = append(methodNames, m.Name)
					}
				}
				if len(methodNames) > 0 {
					// Ищем методы с такими же именами
					// Берем первые несколько методов для поиска кандидатов
					maxMethodsToCheck := 3
					if len(methodNames) < maxMethodsToCheck {
						maxMethodsToCheck = len(methodNames)
					}

					for i := 0; i < maxMethodsToCheck; i++ {
						candidates, err := s.st.FindASTUnits(ctx, methodNames[i], "method", "go", 100)
						if err != nil {
							continue
						}
						for _, cand := range candidates {
							if cand.ParentID.Valid {
								owner, err := s.st.GetASTUnit(ctx, cand.ParentID.Int64)
								if err == nil && owner != nil && (owner.Kind == "struct" || owner.Kind == "type") {
									if seen[owner.ID] {
										continue
									}
									// Проверяем, что у этой структуры есть все остальные методы интерфейса
									ownerMethods, err := s.st.ChildrenOf(ctx, owner.ID)
									if err == nil {
										ownerMethodNames := make(map[string]bool)
										for _, om := range ownerMethods {
											ownerMethodNames[om.Name] = true
										}
										matchCount := 0
										for _, mn := range methodNames {
											if ownerMethodNames[mn] {
												matchCount++
											}
										}
										if matchCount == len(methodNames) {
											seen[owner.ID] = true
											out = append(out, *owner)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// 2. По имени (для неразрешённых связей или если интерфейс не найден как юнит).
	// Если мы знаем язык(и) найденных интерфейсов — ограничиваем поиск ими,
	// чтобы Java-implements Foo не подтягивало Go-тип Foo и наоборот.
	langs := map[string]bool{}
	for _, u := range units {
		if u.Language != "" {
			langs[u.Language] = true
		}
	}
	var byNameAll []store.Edge
	if len(langs) == 0 {
		// Интерфейс не найден как юнит — fallback на старое поведение
		// (без языкового фильтра, иначе вообще ничего не найдём).
		byNameAll, err = s.st.EdgesByDstName(ctx, iface, graph.EdgeImplements)
		if err != nil {
			return nil, err
		}
	} else {
		for lang := range langs {
			part, err := s.st.EdgesByDstNameForLang(ctx, iface, graph.EdgeImplements, lang)
			if err != nil {
				return nil, err
			}
			byNameAll = append(byNameAll, part...)
		}
	}
	for _, e := range byNameAll {
		if seen[e.SrcID] {
			continue
		}
		u, err := s.st.GetASTUnit(ctx, e.SrcID)
		if err != nil || u == nil {
			continue
		}
		// Для Go исключаем интерфейсы из списка реализаций
		if u.Language == "go" && u.Kind == "interface" {
			continue
		}
		seen[u.ID] = true
		out = append(out, *u)
	}

	return out, nil
}

func (s *Service) FindCallers(ctx context.Context, function string) ([]store.ASTUnit, error) {
	defs, err := s.findCallable(ctx, function)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	out := []store.ASTUnit{}

	// 1. По ID определений
	for _, d := range defs {
		cs, err := s.g.Callers(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			out = append(out, c)
		}
	}

	// 2. По имени (для неразрешённых вызовов, например методов Go).
	// Ограничиваем поиск языками найденных определений, чтобы избежать
	// кросс-языковых ложных совпадений (TS-вызов log() ↔ Go-функция log).
	langs := map[string]bool{}
	for _, d := range defs {
		if d.Language != "" {
			langs[d.Language] = true
		}
	}
	var byNameAll []store.Edge
	if len(langs) == 0 {
		byNameAll, err = s.st.EdgesByDstName(ctx, function, graph.EdgeCall)
		if err != nil {
			return nil, err
		}
	} else {
		for lang := range langs {
			part, err := s.st.EdgesByDstNameForLang(ctx, function, graph.EdgeCall, lang)
			if err != nil {
				return nil, err
			}
			byNameAll = append(byNameAll, part...)
		}
	}
	for _, e := range byNameAll {
		if seen[e.SrcID] {
			continue
		}
		u, err := s.st.GetASTUnit(ctx, e.SrcID)
		if err != nil || u == nil {
			continue
		}
		seen[u.ID] = true
		out = append(out, *u)
	}

	return out, nil
}

func (s *Service) FindCallees(ctx context.Context, function string) ([]store.ASTUnit, error) {
	defs, err := s.findCallable(ctx, function)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	out := []store.ASTUnit{}
	for _, d := range defs {
		cs, err := s.g.Callees(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *Service) findCallable(ctx context.Context, name string) ([]store.ASTUnit, error) {
	all := []store.ASTUnit{}
	for _, k := range []string{"function", "method"} {
		us, err := s.st.FindASTUnits(ctx, name, k, "", 20)
		if err != nil {
			return nil, err
		}
		all = append(all, us...)
	}
	return all, nil
}

// ----- Context retrieval -----

// SurroundingContext возвращает текст вокруг unit: его собственный body
// плюс beforeLines/afterLines дополнительных строк за пределами.
func (s *Service) SurroundingContext(ctx context.Context, id int64, beforeLines, afterLines int) (string, error) {
	u, err := s.st.GetASTUnit(ctx, id)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", ErrNotFound
	}
	if u.FilePath == "" {
		return "", nil
	}
	src, err := os.ReadFile(u.FilePath)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(src), "\n")
	start := u.StartLine - 1 - beforeLines
	end := u.EndLine + afterLines
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n"), nil
}

// RelatedFiles — файлы, связанные с символом через import/call/reference.
func (s *Service) RelatedFiles(ctx context.Context, id int64) ([]string, error) {
	// 1. Симметричное окружение глубины 1 по основным рёбрам.
	nb, err := s.g.ExpandNeighbors(ctx, id, 1, []string{graph.EdgeCall, graph.EdgeImport, graph.EdgeReference, graph.EdgeImplements, graph.EdgeExtends})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, n := range nb.Nodes {
		if n.FilePath != "" {
			seen[n.FilePath] = true
		}
	}
	// 2. Файлы, упомянутые в самих edges (для unresolved/external dst).
	for _, e := range nb.Edges {
		if e.FilePath != "" {
			seen[e.FilePath] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// SimilarCode — делегирует SimilarSearcher (vector), если он подключён.
func (s *Service) SimilarCode(ctx context.Context, id int64, limit int) ([]store.ASTUnit, error) {
	if s.sim == nil {
		return []store.ASTUnit{}, nil
	}
	u, err := s.st.GetASTUnit(ctx, id)
	if err != nil || u == nil {
		return nil, err
	}
	return s.sim.SimilarToUnit(ctx, *u, limit)
}
