package astindex

// Файл содержит Go-специфичный экстрактор AST units и edges на базе
// стандартной библиотеки go/ast + go/parser.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"database/sql"

	"ragota/internal/store"
)

// pendingEdge — промежуточное представление ребра до резолва dst_id.
type pendingEdge struct {
	srcIdx  int
	dstIdx  int    // -1 если dst не в этом файле
	dstName string // qualified или name для отложенного резолва
	kind    string
	line    int
}

// parseGo разбирает Go-файл и возвращает список AST units и pendingEdge.
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

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			i.addFuncDecl(fset, src, d, pkgName, moduleIdx, &units, &edges)
		case *ast.GenDecl:
			i.addGenDecl(fset, src, d, pkgName, moduleIdx, &units, &edges)
		}
	}

	// Post-process: fix ParentID for methods.
	typeIdx := make(map[string]int)
	for idx, u := range units {
		if u.Kind == "struct" || u.Kind == "interface" || u.Kind == "type" {
			typeIdx[u.Name] = idx
		}
	}
	for idx, u := range units {
		if u.Kind == "method" {
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

	if d.Recv != nil && len(d.Recv.List) > 0 {
		kind = "method"
		recvType := exprName(d.Recv.List[0].Type)
		if recvType != "" {
			qualified = pkg + "." + recvType + "." + name
		}
	}

	declStart := d.Pos()
	if d.Doc != nil && d.Doc.Pos().IsValid() {
		declStart = d.Doc.Pos()
	}
	startPos := fset.Position(declStart)
	endPos := fset.Position(d.End())
	namePos := fset.Position(d.Name.Pos())
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
		ParentID:      sql.NullInt64{Int64: int64(parentIdx), Valid: true},
		StartLine:     startPos.Line,
		EndLine:       endPos.Line,
		StartByte:     startByte,
		EndByte:       endByte,
		NameStartLine: namePos.Line,
		NameStartCol:  namePos.Column - 1,
		Signature:     signatureOf(src, d),
		Doc:           commentText(d.Doc),
		Hash:          hashBytes([]byte(body)),
	})

	if d.Body != nil {
		ast.Inspect(d.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				callee := exprName(node.Fun)
				if callee != "" {
					*edges = append(*edges, pendingEdge{
						srcIdx: idx, dstIdx: -1, dstName: callee, kind: "call",
						line: fset.Position(node.Pos()).Line,
					})
				}
			case *ast.SelectorExpr:
				if nm := exprName(node); nm != "" {
					*edges = append(*edges, pendingEdge{
						srcIdx: idx, dstIdx: -1, dstName: nm, kind: "reference",
						line: fset.Position(node.Pos()).Line,
					})
				}
			case *ast.Ident:
				if node.Obj == nil {
					*edges = append(*edges, pendingEdge{
						srcIdx: idx, dstIdx: -1, dstName: node.Name, kind: "reference",
						line: fset.Position(node.Pos()).Line,
					})
				}
			}
			return true
		})
	}

	extractImplementsEdges(commentText(d.Doc), idx, startPos.Line, edges)
}
