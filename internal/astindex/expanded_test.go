package astindex

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ragota/internal/config"
	"ragota/internal/store"
)

// ==================== detectLang expanded ====================

func TestDetectLang_AllSupported(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"main.go", "go"},
		{"app.ts", "typescript"},
		{"component.tsx", "typescript"},
		{"script.js", "javascript"},
		{"module.mjs", "javascript"},
		{"config.cjs", "javascript"},
		{"app.jsx", "javascript"},
		{"main.py", "python"},
		{"Service.java", "java"},
		// Case insensitive
		{"MAIN.GO", "go"},
		{"app.TS", "typescript"},
		{"app.JS", "javascript"},
		{"MAIN.PY", "python"},
		{"Service.JAVA", "java"},
		// Unsupported
		{"README.md", ""},
		{"Makefile", ""},
		{"Dockerfile", ""},
		{"config.yaml", ""},
		{"data.json", ""},
		{"style.css", ""},
		{"index.html", ""},
		{"noext", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, detectLang(tt.path))
		})
	}
}

// ==================== firstLine expanded ====================

func TestFirstLine_Empty(t *testing.T) {
	assert.Equal(t, "", firstLine(""))
}

func TestFirstLine_SingleLine(t *testing.T) {
	assert.Equal(t, "hello", firstLine("hello"))
}

func TestFirstLine_WithNewline(t *testing.T) {
	assert.Equal(t, "first", firstLine("first\nsecond"))
}

func TestFirstLine_LeadingTrailingSpaces(t *testing.T) {
	assert.Equal(t, "trimmed", firstLine("  trimmed  \nmore"))
}

func TestFirstLine_VeryLong(t *testing.T) {
	long := strings.Repeat("a", 300)
	result := firstLine(long)
	assert.Equal(t, 203, len(result)) // 200 + "..."
	assert.True(t, strings.HasSuffix(result, "..."))
}

func TestFirstLine_ExactlyMaxLength(t *testing.T) {
	exact := strings.Repeat("b", 200)
	result := firstLine(exact)
	assert.Equal(t, 200, len(result))
	assert.Equal(t, exact, result)
}

// ==================== hashBytes expanded ====================

func TestHashBytes_Empty(t *testing.T) {
	result := hashBytes([]byte{})
	assert.Len(t, result, 40)
}

func TestHashBytes_Deterministic(t *testing.T) {
	a := hashBytes([]byte("test content"))
	b := hashBytes([]byte("test content"))
	assert.Equal(t, a, b)
}

func TestHashBytes_DifferentInputs(t *testing.T) {
	a := hashBytes([]byte("input1"))
	b := hashBytes([]byte("input2"))
	assert.NotEqual(t, a, b)
}

func TestHashBytes_LargeInput(t *testing.T) {
	large := make([]byte, 1024*1024) // 1MB
	result := hashBytes(large)
	assert.Len(t, result, 40)
}

// ==================== commentText expanded ====================

func TestCommentText_Nil(t *testing.T) {
	assert.Equal(t, "", commentText(nil))
}

func TestCommentText_SingleLine(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", `package test
// Hello is a function
func Hello() {}
`, parser.ParseComments)
	require.NoError(t, err)
	require.Len(t, f.Decls, 1)
	fn := f.Decls[0].(*ast.FuncDecl)
	result := commentText(fn.Doc)
	assert.Contains(t, result, "Hello is a function")
}

func TestCommentText_MultiLine(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", `package test
/*
Multi-line comment
with multiple lines
*/
func Hello() {}
`, parser.ParseComments)
	require.NoError(t, err)
	require.Len(t, f.Decls, 1)
	fn := f.Decls[0].(*ast.FuncDecl)
	result := commentText(fn.Doc)
	assert.Contains(t, result, "Multi-line comment")
}

// ==================== exprName expanded ====================

func TestExprName_Ident(t *testing.T) {
	e := &ast.Ident{Name: "Foo"}
	assert.Equal(t, "Foo", exprName(e))
}

func TestExprName_SelectorExpr(t *testing.T) {
	e := &ast.SelectorExpr{
		X:   &ast.Ident{Name: "pkg"},
		Sel: &ast.Ident{Name: "Func"},
	}
	assert.Equal(t, "pkg.Func", exprName(e))
}

func TestExprName_StarExpr(t *testing.T) {
	e := &ast.StarExpr{X: &ast.Ident{Name: "MyStruct"}}
	assert.Equal(t, "MyStruct", exprName(e))
}

func TestExprName_IndexExpr(t *testing.T) {
	e := &ast.IndexExpr{
		X:     &ast.Ident{Name: "Map"},
		Index: &ast.Ident{Name: "string"},
	}
	assert.Equal(t, "Map", exprName(e))
}

func TestExprName_IndexListExpr(t *testing.T) {
	e := &ast.IndexListExpr{
		X:       &ast.Ident{Name: "Pair"},
		Indices: []ast.Expr{&ast.Ident{Name: "A"}, &ast.Ident{Name: "B"}},
	}
	assert.Equal(t, "Pair", exprName(e))
}

func TestExprName_UnknownType(t *testing.T) {
	e := &ast.BasicLit{Kind: token.STRING, Value: `"hello"`}
	assert.Equal(t, "", exprName(e))
}

func TestExprName_NilExpr(t *testing.T) {
	assert.Equal(t, "", exprName(nil))
}

// ==================== signatureOf expanded ====================

func TestSignatureOf_SimpleFunc(t *testing.T) {
	src := []byte(`package test

func Hello(name string) string {
	return "hi " + name
}
`)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, 0)
	require.NoError(t, err)
	require.Len(t, f.Decls, 1)
	fn := f.Decls[0].(*ast.FuncDecl)
	sig := signatureOf(src, fn)
	assert.Contains(t, sig, "func Hello")
	assert.Contains(t, sig, "string")
	assert.NotContains(t, sig, "return")
}

func TestSignatureOf_NoBody(t *testing.T) {
	src := []byte(`package test

func Forward()
`)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		// Expected — Go parser may error on func without body outside interface
		return
	}
	if len(f.Decls) > 0 {
		if fn, ok := f.Decls[0].(*ast.FuncDecl); ok {
			sig := signatureOf(src, fn)
			_ = sig // just ensure no panic
		}
	}
}

// ==================== parseGo expanded ====================

func TestParseGo_MultipleFunctions(t *testing.T) {
	src := `package demo

func A() {}
func B() {}
func C() {}
`
	idx := &Indexer{}
	units, _, err := idx.parseGo("multi.go", []byte(src))
	require.NoError(t, err)

	funcCount := 0
	for _, u := range units {
		if u.Kind == "function" {
			funcCount++
		}
	}
	assert.Equal(t, 3, funcCount)
}

func TestParseGo_GenericFunc(t *testing.T) {
	src := `package demo

func Min[T ~int | ~float64](a, b T) T {
	if a < b {
		return a
	}
	return b
}
`
	idx := &Indexer{}
	units, _, err := idx.parseGo("generic.go", []byte(src))
	require.NoError(t, err)

	found := false
	for _, u := range units {
		if u.Kind == "function" && u.Name == "Min" {
			found = true
		}
	}
	assert.True(t, found, "should find generic function Min")
}

func TestParseGo_InterfaceEmbedding(t *testing.T) {
	src := `package demo

import "io"

type ReadWriter interface {
	io.Reader
	io.Writer
	Extra() error
}
`
	idx := &Indexer{}
	units, edges, err := idx.parseGo("embed.go", []byte(src))
	require.NoError(t, err)

	iface := findASTUnit(units, "interface", "ReadWriter")
	require.NotNil(t, iface)

	// Should have extends edges for io.Reader and io.Writer
	hasReader := false
	hasWriter := false
	for _, e := range edges {
		if e.kind == "extends" {
			if strings.Contains(e.dstName, "Reader") {
				hasReader = true
			}
			if strings.Contains(e.dstName, "Writer") {
				hasWriter = true
			}
		}
	}
	assert.True(t, hasReader, "should have extends edge for io.Reader")
	assert.True(t, hasWriter, "should have extends edge for io.Writer")
}

func TestParseGo_VarBlock(t *testing.T) {
	src := `package demo

var (
	X = 1
	Y = "hello"
	Z = true
)
`
	idx := &Indexer{}
	units, _, err := idx.parseGo("vars.go", []byte(src))
	require.NoError(t, err)

	varCount := 0
	for _, u := range units {
		if u.Kind == "variable" {
			varCount++
		}
	}
	assert.Equal(t, 3, varCount)
}

func TestParseGo_ConstBlock(t *testing.T) {
	src := `package demo

const (
	A = 1
	B = 2
	C = 3
)
`
	idx := &Indexer{}
	units, _, err := idx.parseGo("consts.go", []byte(src))
	require.NoError(t, err)

	constCount := 0
	for _, u := range units {
		if u.Kind == "constant" {
			constCount++
		}
	}
	assert.Equal(t, 3, constCount)
}

func TestParseGo_MethodOnPointerType(t *testing.T) {
	src := `package demo

type Server struct{}

func (s *Server) Start() error {
	return nil
}
`
	idx := &Indexer{}
	units, _, err := idx.parseGo("method.go", []byte(src))
	require.NoError(t, err)

	m := findASTUnit(units, "method", "Start")
	require.NotNil(t, m)
	assert.Equal(t, "demo.Server.Start", m.Qualified)
}

func TestParseGo_AliasedImport(t *testing.T) {
	src := `package demo

import (
	myfmt "fmt"
)

func Hello() {
	myfmt.Println("hello")
}
`
	idx := &Indexer{}
	units, edges, err := idx.parseGo("alias.go", []byte(src))
	require.NoError(t, err)
	_ = units

	// Should have import edge for fmt (or myfmt)
	hasImport := false
	for _, e := range edges {
		if e.kind == "import" {
			hasImport = true
		}
	}
	assert.True(t, hasImport)
}

// ==================== Indexer integration ====================

func TestNew(t *testing.T) {
	cfg := config.Default()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	idx := New(cfg, st)
	assert.NotNil(t, idx)
	assert.NotNil(t, idx.ts)
	assert.NotNil(t, idx.matcher)
}

func TestIndexer_IndexFile_GoFile(t *testing.T) {
	cfg := config.Default()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(goFile, []byte(`package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`), 0644))

	idx := New(cfg, st)
	err = idx.IndexFile(context.Background(), goFile)
	require.NoError(t, err)

	// Verify units were created
	units, err := st.ListASTUnitsByFile(context.Background(), goFile)
	require.NoError(t, err)
	assert.NotEmpty(t, units)
}

func TestIndexer_IndexFile_SkipsUnchanged(t *testing.T) {
	cfg := config.Default()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	content := []byte(`package main

func Hello() string {
	return "hello"
}
`)
	require.NoError(t, os.WriteFile(goFile, content, 0644))

	idx := New(cfg, st)
	err = idx.IndexFile(context.Background(), goFile)
	require.NoError(t, err)

	// Second call should skip (hash unchanged)
	err = idx.IndexFile(context.Background(), goFile)
	require.NoError(t, err)
}

func TestIndexer_RemoveFile(t *testing.T) {
	cfg := config.Default()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(goFile, []byte(`package main

func Hello() {}
`), 0644))

	idx := New(cfg, st)
	require.NoError(t, idx.IndexFile(context.Background(), goFile))

	err = idx.RemoveFile(context.Background(), goFile)
	require.NoError(t, err)

	units, err := st.ListASTUnitsByFile(context.Background(), goFile)
	require.NoError(t, err)
	assert.Empty(t, units)
}

func TestIndexer_IndexFile_NonexistentFile(t *testing.T) {
	cfg := config.Default()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	idx := New(cfg, st)
	err = idx.IndexFile(context.Background(), "/nonexistent/file.go")
	assert.Error(t, err)
}

// Helper
func findASTUnit(units []store.ASTUnit, kind, name string) *store.ASTUnit {
	for i := range units {
		if units[i].Kind == kind && units[i].Name == name {
			return &units[i]
		}
	}
	return nil
}
