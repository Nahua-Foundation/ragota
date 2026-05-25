package astindex

// Файл содержит Go-специфичный экстрактор AST units и edges на базе
// стандартной библиотеки go/ast + go/parser. Edges пока ограничены
// тремя типами: import (module → import path), call (func → callee),
// reference/extends (embedded типы у struct/interface).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"database/sql"

	"ragota/internal/store"
)

// pendingEdge — промежуточное представление ребра до резолва dst_id.
// dstIdx используется, если dst известен и находится в этом же файле;
// иначе dstName хранит qualified/локальное имя для отложенного резолва
// в store.ResolvePendingEdges.
type pendingEdge struct {
	srcIdx  int    // индекс src в массиве units (внутри одного файла)
	dstIdx  int    // -1 если dst не в этом файле
	dstName string // qualified или name для отложенного резолва
	kind    string
	line    int
}

// parseGo разбирает Go-файл и возвращает список AST units (module/func/
// method/type/struct/interface/variable/constant) и список pendingEdge.
func (i *Indexer) parseGo(path string, src []byte) ([]store.ASTUnit, []pendingEdge, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}

	pkgName := ""
	if f.Name != nil {
		pkgName = f.Name.Name
	}

	var units []store.ASTUnit
	var edges []pendingEdge

	// 1. module unit — представляет файл (или package) целиком.
	moduleIdx := len(units)
	units = append(units, store.ASTUnit{
		FilePath:  path,
		Language:  "go",
		Kind:      "module",
		Name:      pkgName,
		Qualified: pkgName,
		StartLine: 1,
		EndLine:   fset.Position(f.End()).Line,
		StartByte: 0,
		EndByte:   len(src),
		Signature: "package " + pkgName,
		Hash:      hashBytes(src),
	})

	// 2. imports — рёбра module --import--> "<import path>".
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		ipath := strings.Trim(imp.Path.Value, "\"`")
		edges = append(edges, pendingEdge{
			srcIdx:  moduleIdx,
			dstIdx:  -1,
			dstName: ipath,
			kind:    "import",
			line:    fset.Position(imp.Pos()).Line,
		})
	}

	// 3. top-level declarations.
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			i.addFuncDecl(fset, src, d, pkgName, moduleIdx, &units, &edges)
		case *ast.GenDecl:
			i.addGenDecl(fset, src, d, pkgName, moduleIdx, &units, &edges)
		}
	}

	// Post-process units to fix ParentID for methods.
	typeIdx := make(map[string]int)
	for idx, u := range units {
		if u.Kind == "struct" || u.Kind == "interface" || u.Kind == "type" {
			typeIdx[u.Name] = idx
		}
	}
	for idx, u := range units {
		if u.Kind == "method" {
			// В Go qualified name для метода: pkg.Type.Method
			parts := strings.Split(u.Qualified, ".")
			if len(parts) >= 3 {
				typeName := parts[len(parts)-2]
				if tIdx, ok := typeIdx[typeName]; ok {
					units[idx].ParentID = sql.NullInt64{Int64: int64(tIdx), Valid: true}
				}
			}
		}
	}

	return units, edges, nil
}

// addFuncDecl добавляет unit для функции/метода и call-edges из её тела.
func (i *Indexer) addFuncDecl(fset *token.FileSet, src []byte, d *ast.FuncDecl, pkg string, moduleIdx int, units *[]store.ASTUnit, edges *[]pendingEdge) {
	if d.Name == nil {
		return
	}
	name := d.Name.Name
	kind := "function"
	parentIdx := moduleIdx
	qualified := pkg + "." + name

	// Метод?
	if d.Recv != nil && len(d.Recv.List) > 0 {
		kind = "method"
		recvType := exprName(d.Recv.List[0].Type)
		if recvType != "" {
			qualified = pkg + "." + recvType + "." + name
		}
	}

	// Расширяем byte-range на doc-комментарий, чтобы chunker и
	// embedder увидели сопроводительные комментарии вместе с телом.
	declStart := d.Pos()
	if d.Doc != nil && d.Doc.Pos().IsValid() {
		declStart = d.Doc.Pos()
	}
	startPos := fset.Position(declStart)
	endPos := fset.Position(d.End())
	namePos := fset.Position(d.Name.Pos())
	startByte := int(declStart - 1) // token positions are 1-based
	endByte := int(d.End() - 1)
	if startByte < 0 {
		startByte = 0
	}
	if endByte > len(src) {
		endByte = len(src)
	}

	body := ""
	if startByte < endByte {
		body = string(src[startByte:endByte])
	}

	idx := len(*units)
	*units = append(*units, store.ASTUnit{
		FilePath:      fset.Position(d.Pos()).Filename,
		Language:      "go",
		Kind:          kind,
		Name:          name,
		Qualified:     qualified,
		ParentID:      sql.NullInt64{Int64: int64(parentIdx), Valid: true},
		StartLine:     startPos.Line,
		EndLine:       endPos.Line,
		StartByte:     startByte,
		EndByte:       endByte,
		NameStartLine: namePos.Line,
		NameStartCol:  namePos.Column - 1, // position is 1-based, we want 0-based for LSP
		Signature:     signatureOf(src, d),
		Doc:           commentText(d.Doc),
		Hash:          hashBytes([]byte(body)),
	})

	// Call/Reference edges внутри тела.
	if d.Body != nil {
		ast.Inspect(d.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				callee := exprName(node.Fun)
				if callee != "" {
					*edges = append(*edges, pendingEdge{
						srcIdx:  idx,
						dstIdx:  -1,
						dstName: callee,
						kind:    "call",
						line:    fset.Position(node.Pos()).Line,
					})
				}
			case *ast.SelectorExpr:
				// pkg.Symbol или obj.Method
				if nm := exprName(node); nm != "" {
					*edges = append(*edges, pendingEdge{
						srcIdx:  idx,
						dstIdx:  -1,
						dstName: nm,
						kind:    "reference",
						line:    fset.Position(node.Pos()).Line,
					})
				}
			case *ast.Ident:
				// Просто символ (если это не определение и не часть селектора выше)
				if node.Obj == nil { // Обычно nil для внешних или если parser.ParseFile был без резолва
					*edges = append(*edges, pendingEdge{
						srcIdx:  idx,
						dstIdx:  -1,
						dstName: node.Name,
						kind:    "reference",
						line:    fset.Position(node.Pos()).Line,
					})
				}
			}
			return true
		})
	}

	// Heuristic: // implements InterfaceName in method comments
	if doc := commentText(d.Doc); doc != "" {
		for _, l := range strings.Split(doc, "\n") {
			l = strings.TrimSpace(strings.ToLower(l))
			if strings.HasPrefix(l, "implements ") {
				iface := strings.TrimSpace(l[len("implements "):])
				if iface != "" {
					*edges = append(*edges, pendingEdge{
						srcIdx:  idx,
						dstIdx:  -1,
						dstName: iface,
						kind:    "implements",
						line:    startPos.Line,
					})
				}
			}
		}
	}
}

// addGenDecl добавляет units для type/struct/interface/var/const деклараций
// и rec/extends-edges для embedded типов.
func (i *Indexer) addGenDecl(fset *token.FileSet, src []byte, d *ast.GenDecl, pkg string, moduleIdx int, units *[]store.ASTUnit, edges *[]pendingEdge) {
	for _, spec := range d.Specs {
		switch ts := spec.(type) {
		case *ast.TypeSpec:
			name := ts.Name.Name
			qualified := pkg + "." + name
			kind := "type"
			switch ts.Type.(type) {
			case *ast.StructType:
				kind = "struct"
			case *ast.InterfaceType:
				kind = "interface"
			}

			declStart := d.Pos()
			if d.Doc != nil && d.Doc.Pos().IsValid() {
				declStart = d.Doc.Pos()
			}
			startPos := fset.Position(declStart)
			endPos := fset.Position(d.End())
			namePos := fset.Position(ts.Name.Pos())
			startByte := int(declStart - 1)
			endByte := int(d.End() - 1)
			if startByte < 0 {
				startByte = 0
			}
			if endByte > len(src) {
				endByte = len(src)
			}
			body := ""
			if startByte < endByte {
				body = string(src[startByte:endByte])
			}

			idx := len(*units)
			*units = append(*units, store.ASTUnit{
				FilePath:      fset.Position(d.Pos()).Filename,
				Language:      "go",
				Kind:          kind,
				Name:          name,
				Qualified:     qualified,
				ParentID:      sql.NullInt64{Int64: int64(moduleIdx), Valid: true},
				StartLine:     startPos.Line,
				EndLine:       endPos.Line,
				StartByte:     startByte,
				EndByte:       endByte,
				NameStartLine: namePos.Line,
				NameStartCol:  namePos.Column - 1,
				Signature:     firstLine(body),
				Doc:           commentText(d.Doc),
				Hash:          hashBytes([]byte(body)),
			})

			switch tt := ts.Type.(type) {
			case *ast.StructType:
				if tt.Fields != nil {
					for _, f := range tt.Fields.List {
						if nm := exprName(f.Type); nm != "" {
							kind := "reference"
							if len(f.Names) == 0 {
								// embedded type in struct is still a reference, but we can call it reference
							}
							*edges = append(*edges, pendingEdge{
								srcIdx:  idx,
								dstIdx:  -1,
								dstName: nm,
								kind:    kind,
								line:    fset.Position(f.Pos()).Line,
							})
						}
					}
				}
			case *ast.InterfaceType:
				if tt.Methods != nil {
					for _, f := range tt.Methods.List {
						if len(f.Names) > 0 {
							// Метод интерфейса
							for _, mName := range f.Names {
								mStart := fset.Position(f.Pos())
								mEnd := fset.Position(f.End())
								mNamePos := fset.Position(mName.Pos())
								mBody := ""
								if int(f.Pos()-1) < int(f.End()-1) && int(f.End()-1) <= len(src) {
									mBody = string(src[f.Pos()-1 : f.End()-1])
								}
								*units = append(*units, store.ASTUnit{
									FilePath:      fset.Position(f.Pos()).Filename,
									Language:      "go",
									Kind:          "method", // или "function"? обычно "method" для интерфейса
									Name:          mName.Name,
									Qualified:     qualified + "." + mName.Name,
									ParentID:      sql.NullInt64{Int64: int64(idx), Valid: true},
									StartLine:     mStart.Line,
									EndLine:       mEnd.Line,
									StartByte:     int(f.Pos() - 1),
									EndByte:       int(f.End() - 1),
									NameStartLine: mNamePos.Line,
									NameStartCol:  mNamePos.Column - 1,
									Signature:     firstLine(mBody),
									Hash:          hashBytes([]byte(mBody)),
								})
							}
						} else if nm := exprName(f.Type); nm != "" { // embedded interface
							*edges = append(*edges, pendingEdge{
								srcIdx:  idx,
								dstIdx:  -1,
								dstName: nm,
								kind:    "extends",
								line:    fset.Position(f.Pos()).Line,
							})
						}
					}
				}
			}

			// Embedded типы у struct → reference; embedded interfaces → extends.
			// Пытаемся также найти реализации интерфейсов через комментарии вида "// implements InterfaceName"
			docText := commentText(d.Doc)
			if docText != "" {
				lines := strings.Split(docText, "\n")
				for _, l := range lines {
					l = strings.TrimSpace(strings.ToLower(l))
					if strings.HasPrefix(l, "implements ") {
						ifaceName := strings.TrimSpace(l[len("implements "):])
						if ifaceName != "" {
							*edges = append(*edges, pendingEdge{
								srcIdx:  idx,
								dstIdx:  -1,
								dstName: ifaceName,
								kind:    "implements",
								line:    startPos.Line,
							})
						}
					}
				}
			}

		case *ast.ValueSpec:
			kind := "variable"
			if d.Tok == token.CONST {
				kind = "constant"
			}
			for _, nameIdent := range ts.Names {
				name := nameIdent.Name
				qualified := pkg + "." + name

				startPos := fset.Position(ts.Pos())
				endPos := fset.Position(ts.End())
				startByte := int(ts.Pos() - 1)
				endByte := int(ts.End() - 1)
				if startByte < 0 {
					startByte = 0
				}
				if endByte > len(src) {
					endByte = len(src)
				}
				body := string(src[startByte:endByte])

				*units = append(*units, store.ASTUnit{
					FilePath:  fset.Position(ts.Pos()).Filename,
					Language:  "go",
					Kind:      kind,
					Name:      name,
					Qualified: qualified,
					ParentID:  sql.NullInt64{Int64: int64(moduleIdx), Valid: true},
					StartLine: startPos.Line,
					EndLine:   endPos.Line,
					StartByte: startByte,
					EndByte:   endByte,
					Signature: firstLine(body),
					Doc:       commentText(ts.Doc),
					Hash:      hashBytes([]byte(body)),
				})
			}
		}
	}
}
