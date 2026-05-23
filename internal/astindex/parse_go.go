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

	"aitools/internal/store"
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
		FilePath:  fset.Position(d.Pos()).Filename,
		Language:  "go",
		Kind:      kind,
		Name:      name,
		Qualified: qualified,
		ParentID:  sql.NullInt64{Int64: int64(parentIdx), Valid: true},
		StartLine: startPos.Line,
		EndLine:   endPos.Line,
		StartByte: startByte,
		EndByte:   endByte,
		Signature: signatureOf(src, d),
		Doc:       commentText(d.Doc),
		Hash:      hashBytes([]byte(body)),
	})

	// Call edges внутри тела.
	if d.Body != nil {
		ast.Inspect(d.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := exprName(ce.Fun)
			if callee == "" {
				return true
			}
			*edges = append(*edges, pendingEdge{
				srcIdx:  idx,
				dstIdx:  -1,
				dstName: callee,
				kind:    "call",
				line:    fset.Position(ce.Pos()).Line,
			})
			return true
		})
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
				FilePath:  fset.Position(d.Pos()).Filename,
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
				Doc:       commentText(d.Doc),
				Hash:      hashBytes([]byte(body)),
			})

			// Embedded типы у struct → reference; embedded interfaces → extends.
			switch tt := ts.Type.(type) {
			case *ast.StructType:
				if tt.Fields != nil {
					for _, f := range tt.Fields.List {
						if len(f.Names) == 0 { // embedded
							if nm := exprName(f.Type); nm != "" {
								*edges = append(*edges, pendingEdge{
									srcIdx:  idx,
									dstIdx:  -1,
									dstName: nm,
									kind:    "reference",
									line:    fset.Position(f.Pos()).Line,
								})
							}
						}
					}
				}
			case *ast.InterfaceType:
				if tt.Methods != nil {
					for _, f := range tt.Methods.List {
						if len(f.Names) == 0 {
							if nm := exprName(f.Type); nm != "" {
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
