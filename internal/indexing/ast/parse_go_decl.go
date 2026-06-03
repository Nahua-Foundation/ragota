package astindex

import (
	"go/ast"
	"go/token"

	"database/sql"

	"ragota/internal/store"
)

// addGenDecl добавляет units для type/struct/interface/var/const деклараций.
func (i *Indexer) addGenDecl(fset *token.FileSet, src []byte, d *ast.GenDecl, pkg string, moduleIdx int, units *[]store.ASTUnit, edges *[]pendingEdge) {
	for _, spec := range d.Specs {
		switch ts := spec.(type) {
		case *ast.TypeSpec:
			i.addTypeSpec(fset, src, d, ts, pkg, moduleIdx, units, edges)
		case *ast.ValueSpec:
			i.addValueSpec(fset, src, d, ts, pkg, moduleIdx, units)
		}
	}
}

func (i *Indexer) addTypeSpec(fset *token.FileSet, src []byte, d *ast.GenDecl, ts *ast.TypeSpec, pkg string, moduleIdx int, units *[]store.ASTUnit, edges *[]pendingEdge) {
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
					*edges = append(*edges, pendingEdge{
						srcIdx: idx, dstIdx: -1, dstName: nm, kind: "reference",
						line: fset.Position(f.Pos()).Line,
					})
				}
			}
		}
	case *ast.InterfaceType:
		if tt.Methods != nil {
			for _, f := range tt.Methods.List {
				if len(f.Names) > 0 {
					for _, mName := range f.Names {
						mStart := fset.Position(f.Pos())
						mEnd := fset.Position(f.End())
						mNamePos := fset.Position(mName.Pos())
						mBody := ""
						if int(f.Pos()-1) < int(f.End()-1) && int(f.End()-1) <= len(src) {
							mBody = string(src[f.Pos()-1 : f.End()-1])
						}
						*units = append(*units, store.ASTUnit{
							FilePath: fset.Position(f.Pos()).Filename, Language: "go",
							Kind: "method", Name: mName.Name, Qualified: qualified + "." + mName.Name,
							ParentID: sql.NullInt64{Int64: int64(idx), Valid: true},
							StartLine: mStart.Line, EndLine: mEnd.Line,
							StartByte: int(f.Pos() - 1), EndByte: int(f.End() - 1),
							NameStartLine: mNamePos.Line, NameStartCol: mNamePos.Column - 1,
							Signature: firstLine(mBody), Hash: hashBytes([]byte(mBody)),
						})
					}
				} else if nm := exprName(f.Type); nm != "" {
					*edges = append(*edges, pendingEdge{
						srcIdx: idx, dstIdx: -1, dstName: nm, kind: "extends",
						line: fset.Position(f.Pos()).Line,
					})
				}
			}
		}
	}

	extractImplementsEdges(commentText(d.Doc), idx, startPos.Line, edges)
}

func (i *Indexer) addValueSpec(fset *token.FileSet, src []byte, d *ast.GenDecl, ts *ast.ValueSpec, pkg string, moduleIdx int, units *[]store.ASTUnit) {
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
