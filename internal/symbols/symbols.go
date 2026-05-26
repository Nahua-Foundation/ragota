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

	"ragota/internal/graph"
	"ragota/internal/lsp"
	"ragota/internal/store"
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
	mgr *lsp.Manager // LSP для точного поиска (implementations и др.)
}

// New создаёт сервис. sim может быть nil.
func New(st *store.SQLite, g *graph.Service, sim SimilarSearcher) *Service {
	return &Service{st: st, g: g, sim: sim}
}

// SetLSPManager подключает LSP менеджер для уточнения результатов.
func (s *Service) SetLSPManager(mgr *lsp.Manager) { s.mgr = mgr }

// SetSimilarSearcher позднее подключает векторный поиск (после создания).
func (s *Service) SetSimilarSearcher(sim SimilarSearcher) { s.sim = sim }

// ----- AST / structure retrieval -----

func (s *Service) FileSymbols(ctx context.Context, path string) ([]store.ASTUnit, error) {
	return s.st.ListASTUnitsByFile(ctx, path)
}

func (s *Service) Get(ctx context.Context, id int) (*store.ASTUnit, error) {
	return s.st.GetASTUnit(ctx, id)
}

func (s *Service) Parent(ctx context.Context, id int) (*store.ASTUnit, error) {
	u, err := s.st.GetASTUnit(ctx, id)
	if err != nil || u == nil {
		return nil, err
	}
	if !u.ParentID.Valid {
		return nil, nil
	}
	return s.st.GetASTUnit(ctx, int(u.ParentID.Int64))
}

func (s *Service) Children(ctx context.Context, id int) ([]store.ASTUnit, error) {
	return s.st.ChildrenOf(ctx, id)
}

// ----- Symbol-aware lookup -----

func (s *Service) FindDefinition(ctx context.Context, symbol string) ([]store.ASTUnit, error) {
	// Ищем определения (любой kind, кроме module — модуль не «определение символа»).
	// Берем с запасом, так как будем фильтровать модули.
	units, err := s.st.FindASTUnits(ctx, symbol, "", "", "", 100)
	if err != nil {
		return nil, err
	}
	out := []store.ASTUnit{}
	hasExactNonModule := false
	for _, u := range units {
		if u.Kind == "module" {
			continue
		}
		if strings.EqualFold(u.Name, symbol) || strings.EqualFold(u.Qualified, symbol) {
			hasExactNonModule = true
			break
		}
	}

	for _, u := range units {
		if u.Kind == "module" {
			continue
		}
		if hasExactNonModule {
			// Если есть хотя бы одно точное совпадение среди не-модулей, возвращаем только их
			if strings.EqualFold(u.Name, symbol) || strings.EqualFold(u.Qualified, symbol) {
				out = append(out, u)
			}
		} else {
			// Иначе возвращаем всё, что нашли (кроме модулей)
			out = append(out, u)
		}
	}
	return out, nil
}

func (s *Service) FindReferences(ctx context.Context, symbol string) ([]store.Edge, error) {
	// 1. Найти все определения с этим именем (может быть pkg.Func или просто Func).
	defs, err := s.FindDefinition(ctx, symbol)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
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
	kinds := []string{graph.EdgeReference, graph.EdgeImplements, graph.EdgeExtends, graph.EdgeCall}
	if len(namesToSearch) == 0 {
		for _, kind := range kinds {
			byName, err := s.st.EdgesByDstName(ctx, symbol, kind)
			if err != nil {
				continue
			}
			for _, e := range byName {
				if seen[e.ID] {
					continue
				}
				seen[e.ID] = true
				out = append(out, e)
			}
		}

		// Fallback для методов (например, l.log() -> ищем просто log)
		if parts := strings.Split(symbol, "."); len(parts) > 1 {
			lastName := parts[len(parts)-1]
			for _, kind := range kinds {
				byLastName, err := s.st.EdgesByDstName(ctx, lastName, kind)
				if err == nil {
					for _, e := range byLastName {
						if seen[e.ID] {
							continue
						}
						seen[e.ID] = true
						out = append(out, e)
					}
				}
			}
		}

		return out, nil
	}

	for k := range namesToSearch {
		// Ищем все типы рёбер, которые могут считаться "ссылками" на символ
		for _, kind := range kinds {
			byName, err := s.st.EdgesByDstNameForLang(ctx, k.name, kind, k.lang)
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
	}
	return out, nil
}

func (s *Service) FindImplementations(ctx context.Context, iface string) ([]store.ASTUnit, error) {
	// Ищем интерфейс или класс (для JS/TS наследования)
	var units []store.ASTUnit
	for _, k := range []string{"interface", "class", "type", "struct"} {
		us, err := s.st.FindASTUnits(ctx, iface, k, "", "", 50)
		if err == nil {
			units = append(units, us...)
		}
	}
	if len(units) == 0 {
		return nil, nil
	}
	out := []store.ASTUnit{}
	seen := map[int]bool{}

	// 0. Попытка использовать LSP для точного поиска реализаций (если доступен)
	if s.mgr != nil {
		for _, u := range units {
			c, err := s.mgr.EnsureOpen(ctx, u.Language, u.FilePath)
			if err == nil {
				// Метод Implementation в LSP работает по позиции.
				line := u.StartLine - 1
				col := 0
				if u.NameStartLine > 0 {
					line = u.NameStartLine - 1
					col = u.NameStartCol
				}
				locs, err := c.Implementation(ctx, u.FilePath, line, col)
				if err == nil && len(locs) > 0 {
					for _, loc := range locs {
						path := strings.TrimPrefix(loc.URI, "file://")
						// На самом деле проще найти все юниты в этом файле и взять тот, что на этой строке
						fileUnits, _ := s.st.ListASTUnitsByFile(ctx, path)
						for _, fu := range fileUnits {
							// Проверяем, что юнит находится на той же строке, что и результат LSP.
							// LSP возвращает 0-based строки.
							if fu.StartLine == loc.StartLine+1 || fu.NameStartLine == loc.StartLine+1 {
								if !seen[fu.ID] {
									seen[fu.ID] = true
									out = append(out, fu)
								}
							}
						}
					}
				}
			}
		}
	}

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

		// Эвристика для Go/TS/JS: если интерфейс имеет методы, ищем структуры/классы с такими же методами
		if u.Language == "go" || u.Language == "typescript" || u.Language == "javascript" {
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
						candidates, err := s.st.FindASTUnits(ctx, methodNames[i], "method", u.Language, "", 100)
						if err != nil {
							continue
						}
						for _, cand := range candidates {
							if cand.ParentID.Valid {
								owner, err := s.st.GetASTUnit(ctx, int(cand.ParentID.Int64))
								if err == nil && owner != nil {
									// Для Go: struct или type. Для TS: class или interface.
									isPotentialOwner := false
									if u.Language == "go" {
										isPotentialOwner = (owner.Kind == "struct" || owner.Kind == "type")
									} else if u.Language == "typescript" || u.Language == "javascript" {
										isPotentialOwner = (owner.Kind == "class" || owner.Kind == "interface")
									}

									if isPotentialOwner {
										if seen[owner.ID] {
											continue
										}
										// Проверяем, что у этой структуры/класса есть все остальные методы интерфейса
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
	var err error
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
	seen := map[int]bool{}
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

	// 2. По имени (для неразрешённых вызовов, например методов Go/TS/JS).
	// Ограничиваем поиск языками найденных определений, чтобы избежать
	// кросс-языковых ложных совпадений (TS-вызов log() ↔ Go-функция log).
	langs := map[string]bool{}
	for _, d := range defs {
		if d.Language != "" {
			langs[d.Language] = true
		}
	}

	// Если определений не найдено, мы не знаем целевой язык.
	// В этом случае разрешаем поиск по всем языкам (fallback).

	var byNameAll []store.Edge
	if len(langs) == 0 {
		byNameAll, _ = s.st.EdgesByDstName(ctx, function, graph.EdgeCall)
	} else {
		for lang := range langs {
			part, _ := s.st.EdgesByDstNameForLang(ctx, function, graph.EdgeCall, lang)
			byNameAll = append(byNameAll, part...)
		}
	}

	// Всегда делаем fallback для методов (например, Logger.log -> ищем просто log),
	// так как в TS/JS/Go вызовы часто записываются по имени метода без квалификатора.
	if parts := strings.Split(function, "."); len(parts) > 1 {
		lastName := parts[len(parts)-1]
		var byLastName []store.Edge
		if len(langs) == 0 {
			byLastName, _ = s.st.EdgesByDstName(ctx, lastName, graph.EdgeCall)
		} else {
			for lang := range langs {
				part, _ := s.st.EdgesByDstNameForLang(ctx, lastName, graph.EdgeCall, lang)
				byLastName = append(byLastName, part...)
			}
		}
		byNameAll = append(byNameAll, byLastName...)
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
	seen := map[int]bool{}
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

		// Если Callees по графу вернул не всё (например, из-за неразрешенных DstID),
		// пробуем найти цели по имени из эджей этого юнита.
		edges, _ := s.st.EdgesFrom(ctx, d.ID, graph.EdgeCall)
		for _, e := range edges {
			if e.DstID == 0 && e.DstName != "" {
				// Пытаемся найти цели по имени в том же языке.
				// Если имя квалифицированное (pkg.Name), пробуем также по короткому имени.
				names := []string{e.DstName}
				if dot := strings.LastIndex(e.DstName, "."); dot >= 0 {
					names = append(names, e.DstName[dot+1:])
				}

				for _, name := range names {
					targets, _ := s.st.FindASTUnits(ctx, name, "function", d.Language, "", 50)
					meths, _ := s.st.FindASTUnits(ctx, name, "method", d.Language, "", 50)
					targets = append(targets, meths...)
					for _, t := range targets {
						if !seen[t.ID] {
							seen[t.ID] = true
							out = append(out, t)
						}
					}
				}
			}
		}
	}
	return out, nil
}

func (s *Service) findCallable(ctx context.Context, name string) ([]store.ASTUnit, error) {
	all := []store.ASTUnit{}
	for _, k := range []string{"function", "method"} {
		us, err := s.st.FindASTUnits(ctx, name, k, "", "", 100)
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
func (s *Service) SurroundingContext(ctx context.Context, id int, beforeLines, afterLines int) (string, error) {
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
func (s *Service) RelatedFiles(ctx context.Context, id int) ([]string, error) {
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
func (s *Service) SimilarCode(ctx context.Context, id int, limit int) ([]store.ASTUnit, error) {
	if s.sim == nil {
		return []store.ASTUnit{}, nil
	}
	u, err := s.st.GetASTUnit(ctx, id)
	if err != nil || u == nil {
		return nil, err
	}
	return s.sim.SimilarToUnit(ctx, *u, limit)
}
