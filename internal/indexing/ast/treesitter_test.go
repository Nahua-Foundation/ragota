package astindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ragota/pkg/config"
	pkgparser "ragota/internal/indexing/parser"
	"ragota/pkg/state"
	"ragota/internal/store"
)

// ==================== languageGrammar ====================

func TestLanguageGrammar_Java(t *testing.T) {
	lang := languageGrammar("java", "Foo.java")
	assert.NotNil(t, lang)
}

func TestLanguageGrammar_TypeScript(t *testing.T) {
	lang := languageGrammar("typescript", "app.ts")
	assert.NotNil(t, lang)
}

func TestLanguageGrammar_TSX(t *testing.T) {
	lang := languageGrammar("typescript", "component.tsx")
	assert.NotNil(t, lang)
}

func TestLanguageGrammar_JavaScript(t *testing.T) {
	lang := languageGrammar("javascript", "script.js")
	assert.NotNil(t, lang)
}

func TestLanguageGrammar_JSX(t *testing.T) {
	lang := languageGrammar("javascript", "component.jsx")
	assert.NotNil(t, lang)
}

func TestLanguageGrammar_Unknown(t *testing.T) {
	assert.Nil(t, languageGrammar("python", "app.py"))
	assert.Nil(t, languageGrammar("go", "main.go"))
	assert.Nil(t, languageGrammar("rust", "lib.rs"))
	assert.Nil(t, languageGrammar("", "file.txt"))
}

// ==================== isUnitType ====================

func TestIsUnitType_UnitTypes(t *testing.T) {
	unitTypes := []string{
		"class_declaration", "class_definition", "interface_declaration",
		"enum_declaration", "type_declaration", "struct_declaration",
		"type_alias_declaration", "function_declaration", "function_definition",
		"method_definition", "method_declaration", "constructor_declaration",
		"function", "method_signature", "variable_declarator",
		"field_declaration", "module", "module_declaration",
		"internal_module", "namespace_declaration",
	}
	for _, tt := range unitTypes {
		t.Run(tt, func(t *testing.T) {
			assert.True(t, isUnitType(tt))
		})
	}
}

func TestIsUnitType_NonUnitTypes(t *testing.T) {
	nonUnitTypes := []string{
		"comment", "line_comment", "block_comment",
		"import_declaration", "import_statement",
		"expression_statement", "call_expression",
		"if_statement", "return_statement",
		"identifier", "string", "number",
		"", "unknown",
	}
	for _, tt := range nonUnitTypes {
		t.Run(tt, func(t *testing.T) {
			assert.False(t, isUnitType(tt))
		})
	}
}

// ==================== extractJSDocRefs ====================

func TestExtractJSDocRefs_ParamType(t *testing.T) {
	var edges []pendingEdge
	doc := "@param {User} user"
	extractJSDocRefs(doc, 0, &edges, 1)
	assert.NotEmpty(t, edges)
	found := false
	for _, e := range edges {
		if e.dstName == "User" && e.kind == "reference" {
			found = true
		}
	}
	assert.True(t, found, "should find User reference")
}

func TestExtractJSDocRefs_ReturnsType(t *testing.T) {
	var edges []pendingEdge
	doc := "@returns {Promise<User>} the user"
	extractJSDocRefs(doc, 0, &edges, 1)
	found := false
	for _, e := range edges {
		if e.dstName == "User" && e.kind == "reference" {
			found = true
		}
	}
	assert.True(t, found)
}

func TestExtractJSDocRefs_SkipsPrimitives(t *testing.T) {
	var edges []pendingEdge
	doc := "@param {string} name\n@param {number} age\n@param {boolean} flag"
	extractJSDocRefs(doc, 0, &edges, 1)
	assert.Empty(t, edges, "should skip JS primitive types")
}

func TestExtractJSDocRefs_SkipsBuiltins(t *testing.T) {
	var edges []pendingEdge
	doc := "@param {Array} items\n@param {Promise} p\n@param {Map} m\n@param {Set} s"
	extractJSDocRefs(doc, 0, &edges, 1)
	assert.Empty(t, edges, "should skip Array/Promise/Map/Set builtins")
}

func TestExtractJSDocRefs_ComplexType(t *testing.T) {
	var edges []pendingEdge
	doc := "@type {User|null}"
	extractJSDocRefs(doc, 5, &edges, 10)
	found := false
	for _, e := range edges {
		if e.dstName == "User" {
			found = true
			assert.Equal(t, 5, e.srcIdx)
			assert.Equal(t, 10, e.line)
		}
	}
	assert.True(t, found)
}

func TestExtractJSDocRefs_NoTypeAnnotations(t *testing.T) {
	var edges []pendingEdge
	doc := "This is a plain description without JSDoc types."
	extractJSDocRefs(doc, 0, &edges, 1)
	assert.Empty(t, edges)
}

func TestExtractJSDocRefs_EmptyDoc(t *testing.T) {
	var edges []pendingEdge
	extractJSDocRefs("", 0, &edges, 1)
	assert.Empty(t, edges)
}

func TestExtractJSDocRefs_MultipleTypes(t *testing.T) {
	var edges []pendingEdge
	doc := "@param {Foo} f\n@returns {Bar}"
	extractJSDocRefs(doc, 0, &edges, 1)
	names := map[string]bool{}
	for _, e := range edges {
		names[e.dstName] = true
	}
	assert.True(t, names["Foo"])
	assert.True(t, names["Bar"])
}

// ==================== nodeText ====================

func TestNodeText_NilNode(t *testing.T) {
	assert.Equal(t, "", nodeText(nil, []byte("test")))
}

// ==================== parseWithTreeSitter ====================

func TestParseWithTreeSitter_UnknownLang(t *testing.T) {
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "python", "app.py", []byte("x = 1"))
	require.NoError(t, err)
	assert.Nil(t, units)
	assert.Nil(t, edges)
}

func TestParseWithTreeSitter_TypeScript_Function(t *testing.T) {
	src := []byte(`
function greet(name: string): string {
    return "hello " + name;
}
`)
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "typescript", "app.ts", src)
	require.NoError(t, err)
	require.NotEmpty(t, units)

	// Module unit should always be present
	assert.Equal(t, "module", units[0].Kind)
	assert.Equal(t, "typescript", units[0].Language)

	// Should find the function
	foundFunc := false
	for _, u := range units {
		if u.Kind == "function" && u.Name == "greet" {
			foundFunc = true
			assert.Contains(t, u.Qualified, "greet")
		}
	}
	assert.True(t, foundFunc, "should find function greet")
	_ = edges
}

func TestParseWithTreeSitter_TypeScript_Class(t *testing.T) {
	src := []byte(`
class Animal {
    name: string;
    
    constructor(name: string) {
        this.name = name;
    }
    
    speak(): string {
        return this.name + " makes a noise.";
    }
}

class Dog extends Animal {
    breed: string;
    
    speak(): string {
        return this.name + " barks.";
    }
}
`)
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "typescript", "animals.ts", src)
	require.NoError(t, err)

	// Find classes
	classes := map[string]bool{}
	for _, u := range units {
		if u.Kind == "class" {
			classes[u.Name] = true
		}
	}
	assert.True(t, classes["Animal"], "should find class Animal")
	assert.True(t, classes["Dog"], "should find class Dog")

	// Should have extends edge for Dog extends Animal
	hasExtends := false
	for _, e := range edges {
		if e.kind == "extends" {
			hasExtends = true
		}
	}
	assert.True(t, hasExtends, "should have extends edge")
}

func TestParseWithTreeSitter_TypeScript_Interface(t *testing.T) {
	src := []byte(`
interface Greetable {
    greet(): string;
}

interface Animal extends Greetable {
    name: string;
}
`)
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "typescript", "iface.ts", src)
	require.NoError(t, err)

	interfaces := map[string]bool{}
	for _, u := range units {
		if u.Kind == "interface" {
			interfaces[u.Name] = true
		}
	}
	assert.True(t, interfaces["Greetable"])
	assert.True(t, interfaces["Animal"])
	_ = edges
}

func TestParseWithTreeSitter_TypeScript_Import(t *testing.T) {
	src := []byte(`
import { Foo } from './foo';
import * as bar from 'bar-module';

function use(): void {
    const f = new Foo();
    bar.init();
}
`)
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "typescript", "use.ts", src)
	require.NoError(t, err)
	require.NotEmpty(t, units)

	imports := 0
	for _, e := range edges {
		if e.kind == "import" {
			imports++
		}
	}
	assert.GreaterOrEqual(t, imports, 2, "should have at least 2 import edges")
}

func TestParseWithTreeSitter_TypeScript_Require(t *testing.T) {
	src := []byte(`
const fs = require('fs');
const path = require('path');

function readFile(p: string): string {
    return fs.readFileSync(p, 'utf8');
}
`)
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "javascript", "read.js", src)
	require.NoError(t, err)
	require.NotEmpty(t, units)

	importEdges := 0
	for _, e := range edges {
		if e.kind == "import" {
			importEdges++
		}
	}
	assert.GreaterOrEqual(t, importEdges, 2, "should detect require() as import edges")
}

func TestParseWithTreeSitter_Java_BasicClass(t *testing.T) {
	src := []byte(`
package com.example;

import java.util.List;

public class UserService {
    private String name;
    
    public UserService(String name) {
        this.name = name;
    }
    
    public String getName() {
        return this.name;
    }
    
    public void process(List<String> items) {
        for (String item : items) {
            System.out.println(item);
        }
    }
}
`)
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "java", "UserService.java", src)
	require.NoError(t, err)
	require.NotEmpty(t, units)

	// Module should have package as qualified name
	assert.Equal(t, "com.example", units[0].Qualified)
	assert.Equal(t, "java", units[0].Language)

	// Find class
	foundClass := false
	for _, u := range units {
		if u.Kind == "class" && u.Name == "UserService" {
			foundClass = true
			assert.Equal(t, "com.example.UserService", u.Qualified)
		}
	}
	assert.True(t, foundClass, "should find class UserService")

	// Should have import edge
	hasImport := false
	for _, e := range edges {
		if e.kind == "import" {
			hasImport = true
		}
	}
	assert.True(t, hasImport, "should have import edge for java.util.List")
}

func TestParseWithTreeSitter_Java_Interface(t *testing.T) {
	src := []byte(`
package com.example;

public interface Repository {
    void save(Object entity);
    Object findById(String id);
}
`)
	idx := &Indexer{}
	units, _, err := idx.parseWithTreeSitter(context.Background(), "java", "Repository.java", src)
	require.NoError(t, err)

	foundIface := false
	for _, u := range units {
		if u.Kind == "interface" && u.Name == "Repository" {
			foundIface = true
		}
	}
	assert.True(t, foundIface, "should find interface Repository")
}

func TestParseWithTreeSitter_Java_Implements(t *testing.T) {
	src := []byte(`
package com.example;

public class UserRepository implements Repository {
    public void save(Object entity) {}
    public Object findById(String id) { return null; }
}
`)
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "java", "UserRepo.java", src)
	require.NoError(t, err)

	foundClass := false
	for _, u := range units {
		if u.Kind == "class" && u.Name == "UserRepository" {
			foundClass = true
		}
	}
	assert.True(t, foundClass)

	hasImplements := false
	for _, e := range edges {
		if e.kind == "implements" {
			hasImplements = true
		}
	}
	assert.True(t, hasImplements, "should have implements edge")
}

func TestParseWithTreeSitter_JavaScript_Enum(t *testing.T) {
	src := []byte(`
enum Color {
    Red,
    Green,
    Blue
}

function pickColor(): Color {
    return Color.Red;
}
`)
	idx := &Indexer{}
	units, _, err := idx.parseWithTreeSitter(context.Background(), "typescript", "colors.ts", src)
	require.NoError(t, err)

	foundEnum := false
	for _, u := range units {
		if u.Kind == "enum" && u.Name == "Color" {
			foundEnum = true
		}
	}
	assert.True(t, foundEnum, "should find enum Color")
}

func TestParseWithTreeSitter_TypeScript_Variable(t *testing.T) {
	src := []byte(`
const MAX_RETRIES = 3;
let counter = 0;

function increment(): void {
    counter++;
}
`)
	idx := &Indexer{}
	units, _, err := idx.parseWithTreeSitter(context.Background(), "typescript", "vars.ts", src)
	require.NoError(t, err)

	vars := map[string]bool{}
	for _, u := range units {
		if u.Kind == "variable" {
			vars[u.Name] = true
		}
	}
	assert.True(t, vars["MAX_RETRIES"], "should find variable MAX_RETRIES")
	assert.True(t, vars["counter"], "should find variable counter")
}

func TestParseWithTreeSitter_TypeScript_Namespace(t *testing.T) {
	src := []byte(`
namespace MyLib {
    export function init(): void {}
    export class Config {
        debug: boolean;
    }
}
`)
	idx := &Indexer{}
	units, _, err := idx.parseWithTreeSitter(context.Background(), "typescript", "lib.ts", src)
	require.NoError(t, err)

	foundNS := false
	for _, u := range units {
		if u.Kind == "namespace" && u.Name == "MyLib" {
			foundNS = true
		}
	}
	assert.True(t, foundNS, "should find namespace MyLib")
}

func TestParseWithTreeSitter_TypeScript_CallExpressions(t *testing.T) {
	src := []byte(`
function process(): void {
    console.log("processing");
    fetchData();
    const result = transform(data);
}
`)
	idx := &Indexer{}
	_, edges, err := idx.parseWithTreeSitter(context.Background(), "typescript", "calls.ts", src)
	require.NoError(t, err)

	calls := map[string]bool{}
	for _, e := range edges {
		if e.kind == "call" {
			calls[e.dstName] = true
		}
	}
	assert.True(t, calls["fetchData"], "should find call to fetchData")
	assert.True(t, calls["transform"], "should find call to transform")
}

func TestParseWithTreeSitter_JSDocRefs(t *testing.T) {
	src := []byte(`
/**
 * @param {CustomType} input - the input
 * @returns {CustomResult} the result
 */
function process(input: any): any {
    return null;
}
`)
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "typescript", "doc.ts", src)
	require.NoError(t, err)

	// Should find the function
	foundFunc := false
	for _, u := range units {
		if u.Kind == "function" && u.Name == "process" {
			foundFunc = true
		}
	}
	assert.True(t, foundFunc)

	// Should have reference edges from JSDoc
	refs := map[string]bool{}
	for _, e := range edges {
		if e.kind == "reference" {
			refs[e.dstName] = true
		}
	}
	assert.True(t, refs["CustomType"], "should find JSDoc reference to CustomType")
	assert.True(t, refs["CustomResult"], "should find JSDoc reference to CustomResult")
}

func TestParseWithTreeSitter_Java_NoPackage(t *testing.T) {
	src := []byte(`
public class Simple {
    public void run() {}
}
`)
	idx := &Indexer{}
	units, _, err := idx.parseWithTreeSitter(context.Background(), "java", "Simple.java", src)
	require.NoError(t, err)
	require.NotEmpty(t, units)

	// Module should use filename as qualified when no package
	assert.Equal(t, "Simple.java", units[0].Qualified)
}

func TestParseWithTreeSitter_EmptySource(t *testing.T) {
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "typescript", "empty.ts", []byte(""))
	require.NoError(t, err)
	require.Len(t, units, 1, "should still have module unit")
	assert.Equal(t, "module", units[0].Kind)
	assert.Empty(t, edges)
}

func TestParseWithTreeSitter_JSX(t *testing.T) {
	src := []byte(`
import React from 'react';

function App() {
    return <div>Hello</div>;
}

export default App;
`)
	idx := &Indexer{}
	units, _, err := idx.parseWithTreeSitter(context.Background(), "javascript", "App.jsx", src)
	require.NoError(t, err)
	require.NotEmpty(t, units)

	foundFunc := false
	for _, u := range units {
		if u.Kind == "function" && u.Name == "App" {
			foundFunc = true
		}
	}
	assert.True(t, foundFunc, "should find App function in JSX")
}

// ==================== parseGeneric ====================

func TestParseGeneric_EmptyLang(t *testing.T) {
	idx := &Indexer{ts: newTestParser()}
	units, err := idx.parseGeneric(context.Background(), "", "file.unknown", []byte("content"))
	require.NoError(t, err)
	assert.Nil(t, units)
}

func TestParseGeneric_Python(t *testing.T) {
	src := []byte(`
class Calculator:
    def add(self, a, b):
        return a + b

    def subtract(self, a, b):
        return a - b

def standalone(x):
    return x * 2
`)
	idx := &Indexer{ts: newTestParser()}
	units, err := idx.parseGeneric(context.Background(), "python", "calc.py", src)
	require.NoError(t, err)
	require.NotEmpty(t, units)

	// Module unit
	assert.Equal(t, "module", units[0].Kind)
	assert.Equal(t, "calc.py", units[0].Name)

	// Should find class and functions
	classes := map[string]bool{}
	funcs := map[string]bool{}
	for _, u := range units[1:] {
		if u.Kind == "class" {
			classes[u.Name] = true
		}
		if u.Kind == "function" {
			funcs[u.Name] = true
		}
	}
	assert.True(t, classes["Calculator"], "should find class Calculator")
	assert.True(t, funcs["standalone"], "should find standalone function")
}

func TestParseGeneric_PythonParentChild(t *testing.T) {
	src := []byte(`
class MyClass:
    def method_one(self):
        pass
    def method_two(self):
        pass
`)
	idx := &Indexer{ts: newTestParser()}
	units, err := idx.parseGeneric(context.Background(), "python", "mod.py", src)
	require.NoError(t, err)

	// Methods should have parent pointing to class
	classIdx := -1
	for i, u := range units {
		if u.Kind == "class" && u.Name == "MyClass" {
			classIdx = i
		}
	}
	require.Greater(t, classIdx, 0)

	for _, u := range units {
		if u.Name == "method_one" || u.Name == "method_two" {
			assert.True(t, u.ParentID.Valid, "method should have parent")
			assert.Equal(t, int64(classIdx), u.ParentID.Int64, "method parent should be class index")
		}
	}
}

func TestParseGeneric_EmptySource(t *testing.T) {
	idx := &Indexer{ts: newTestParser()}
	units, err := idx.parseGeneric(context.Background(), "python", "empty.py", []byte(""))
	require.NoError(t, err)
	require.Len(t, units, 1, "should have module unit even for empty file")
	assert.Equal(t, "module", units[0].Kind)
}

func TestParseGeneric_QualifiedNames(t *testing.T) {
	src := []byte(`
class Foo:
    def bar(self):
        pass

def baz():
    pass
`)
	idx := &Indexer{ts: newTestParser()}
	units, err := idx.parseGeneric(context.Background(), "python", "test.py", src)
	require.NoError(t, err)

	for _, u := range units {
		if u.Name == "bar" {
			assert.Equal(t, "test.py.Foo.bar", u.Qualified)
		}
		if u.Name == "baz" {
			assert.Equal(t, "test.py.baz", u.Qualified)
		}
	}
}

// ==================== SetBus ====================

func TestSetBus(t *testing.T) {
	idx := &Indexer{}
	assert.Nil(t, idx.bus)

	bus := state.NewBus(t.TempDir())
	idx.SetBus(bus)
	assert.NotNil(t, idx.bus)
	assert.Equal(t, bus, idx.bus)
}

// ==================== Integration: IndexFile with TS/JS/Java ====================

func newTestIndexer(t *testing.T) (*Indexer, *store.SQLite) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	idx := New(cfg, st)
	return idx, st
}

func newTestParser() *pkgparser.Parser {
	return pkgparser.New()
}

func TestIndexFile_TypeScript(t *testing.T) {
	idx, st := newTestIndexer(t)

	dir := t.TempDir()
	tsFile := filepath.Join(dir, "app.ts")
	require.NoError(t, os.WriteFile(tsFile, []byte(`
interface Config {
    host: string;
    port: number;
}

function loadConfig(): Config {
    return { host: "localhost", port: 8080 };
}

class Server {
    config: Config;
    
    constructor(config: Config) {
        this.config = config;
    }
    
    start(): void {
        console.log("starting");
    }
}
`), 0644))

	err := idx.IndexFile(context.Background(), tsFile)
	require.NoError(t, err)

	units, err := st.ListASTUnitsByFile(context.Background(), tsFile)
	require.NoError(t, err)
	assert.NotEmpty(t, units)

	kinds := map[string]int{}
	for _, u := range units {
		kinds[u.Kind]++
	}
	assert.GreaterOrEqual(t, kinds["module"], 1)
	assert.GreaterOrEqual(t, kinds["interface"], 1)
	assert.GreaterOrEqual(t, kinds["function"], 1)
	assert.GreaterOrEqual(t, kinds["class"], 1)
}

func TestIndexFile_JavaScript(t *testing.T) {
	idx, st := newTestIndexer(t)

	dir := t.TempDir()
	jsFile := filepath.Join(dir, "util.js")
	require.NoError(t, os.WriteFile(jsFile, []byte(`
const fs = require('fs');

function readJSON(path) {
    const data = fs.readFileSync(path, 'utf8');
    return JSON.parse(data);
}

class Logger {
    log(msg) {
        console.log(msg);
    }
}
`), 0644))

	err := idx.IndexFile(context.Background(), jsFile)
	require.NoError(t, err)

	units, err := st.ListASTUnitsByFile(context.Background(), jsFile)
	require.NoError(t, err)
	assert.NotEmpty(t, units)
}

func TestIndexFile_Java(t *testing.T) {
	idx, st := newTestIndexer(t)

	dir := t.TempDir()
	javaFile := filepath.Join(dir, "Main.java")
	require.NoError(t, os.WriteFile(javaFile, []byte(`
package com.app;

import java.util.ArrayList;

public class Main {
    public static void main(String[] args) {
        System.out.println("Hello");
    }
}
`), 0644))

	err := idx.IndexFile(context.Background(), javaFile)
	require.NoError(t, err)

	units, err := st.ListASTUnitsByFile(context.Background(), javaFile)
	require.NoError(t, err)
	assert.NotEmpty(t, units)

	// Verify Java-specific: module qualified should be package name
	module := units[0]
	assert.Equal(t, "module", module.Kind)
	assert.Equal(t, "com.app", module.Qualified)
}

func TestIndexFile_Python(t *testing.T) {
	idx, st := newTestIndexer(t)

	dir := t.TempDir()
	pyFile := filepath.Join(dir, "service.py")
	require.NoError(t, os.WriteFile(pyFile, []byte(`
class UserService:
    def __init__(self, db):
        self.db = db
    
    def find_user(self, user_id):
        return self.db.query(user_id)

def create_app():
    return "app"
`), 0644))

	err := idx.IndexFile(context.Background(), pyFile)
	require.NoError(t, err)

	units, err := st.ListASTUnitsByFile(context.Background(), pyFile)
	require.NoError(t, err)
	assert.NotEmpty(t, units)
}

func TestIndexFile_NonexistentFile(t *testing.T) {
	idx, _ := newTestIndexer(t)
	err := idx.IndexFile(context.Background(), "/nonexistent/path/file.ts")
	assert.Error(t, err)
}

func TestIndexFile_SkipUnchanged(t *testing.T) {
	idx, st := newTestIndexer(t)

	dir := t.TempDir()
	tsFile := filepath.Join(dir, "stable.ts")
	content := []byte(`function hello() { return "world"; }`)
	require.NoError(t, os.WriteFile(tsFile, content, 0644))

	require.NoError(t, idx.IndexFile(context.Background(), tsFile))

	units1, err := st.ListASTUnitsByFile(context.Background(), tsFile)
	require.NoError(t, err)

	// Second call should skip
	require.NoError(t, idx.IndexFile(context.Background(), tsFile))

	units2, err := st.ListASTUnitsByFile(context.Background(), tsFile)
	require.NoError(t, err)
	assert.Equal(t, len(units1), len(units2))
}

func TestRemoveFile_TypeScript(t *testing.T) {
	idx, st := newTestIndexer(t)

	dir := t.TempDir()
	tsFile := filepath.Join(dir, "remove.ts")
	require.NoError(t, os.WriteFile(tsFile, []byte(`function foo() {}`), 0644))

	require.NoError(t, idx.IndexFile(context.Background(), tsFile))

	units, err := st.ListASTUnitsByFile(context.Background(), tsFile)
	require.NoError(t, err)
	assert.NotEmpty(t, units)

	require.NoError(t, idx.RemoveFile(context.Background(), tsFile))

	units, err = st.ListASTUnitsByFile(context.Background(), tsFile)
	require.NoError(t, err)
	assert.Empty(t, units)
}

// ==================== FullScan ====================

func TestFullScan_MultiLangFiles(t *testing.T) {
	dir := t.TempDir()

	// Create files of different languages
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

func main() {}
`), 0644))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.ts"), []byte(`
function hello(): string {
    return "hello";
}
`), 0644))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "service.py"), []byte(`
def process():
    pass
`), 0644))

	cfg := config.Default()
	cfg.Root = dir
	cfg.Extensions = []string{".go", ".ts", ".py"}
	cfg.Ignore = []string{}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	idx := New(cfg, st)

	bus := state.NewBus(t.TempDir())
	idx.SetBus(bus)

	err = idx.FullScan(context.Background())
	require.NoError(t, err)

	stats, err := st.GraphStats(context.Background())
	require.NoError(t, err)
	assert.Greater(t, stats.Units, 0, "should have indexed units from multiple files")
}

func TestFullScan_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	cfg := config.Default()
	cfg.Root = dir
	cfg.Extensions = []string{".go", ".ts"}
	cfg.Ignore = []string{}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	idx := New(cfg, st)

	err = idx.FullScan(context.Background())
	require.NoError(t, err)

	stats, err := st.GraphStats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, stats.Units)
}

func TestFullScan_CancelledContext(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main
func main() {}
`), 0644))

	cfg := config.Default()
	cfg.Root = dir
	cfg.Extensions = []string{".go"}
	cfg.Ignore = []string{}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	idx := New(cfg, st)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err = idx.FullScan(ctx)
	assert.Error(t, err)
}

func TestFullScan_SkipsUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stable.go"), []byte(`package stable

func Hello() string { return "hi" }
`), 0644))

	cfg := config.Default()
	cfg.Root = dir
	cfg.Extensions = []string{".go"}
	cfg.Ignore = []string{}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	idx := New(cfg, st)

	require.NoError(t, idx.FullScan(context.Background()))

	stats1, err := st.GraphStats(context.Background())
	require.NoError(t, err)

	// Second scan should skip unchanged file
	require.NoError(t, idx.FullScan(context.Background()))

	stats2, err := st.GraphStats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, stats1.Units, stats2.Units)
}

func TestFullScan_DetectsStaleHashes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main
func main() {}
`), 0644))

	cfg := config.Default()
	cfg.Root = dir
	cfg.Extensions = []string{".go"}
	cfg.Ignore = []string{}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	idx := New(cfg, st)

	// First scan to populate hashes
	require.NoError(t, idx.FullScan(context.Background()))

	// Now delete AST units but keep hashes (simulates stale state)
	ctx := context.Background()
	units, _ := st.ListASTUnitsByFile(ctx, filepath.Join(dir, "main.go"))
	if len(units) > 0 {
		// Clear AST units but keep file hashes
		st.ReplaceASTUnits(ctx, filepath.Join(dir, "main.go"), nil)
		st.ReplaceEdges(ctx, filepath.Join(dir, "main.go"), nil)
	}

	// Verify stale detection: units=0 but hashes exist
	stats, _ := st.GraphStats(ctx)
	if stats.Units == 0 {
		// FullScan should detect stale hashes and reset them
		require.NoError(t, idx.FullScan(context.Background()))

		// After reset + reindex, units should be > 0
		stats2, err := st.GraphStats(context.Background())
		require.NoError(t, err)
		assert.Greater(t, stats2.Units, 0, "should reindex after stale hash reset")
	}
}

// ==================== indexFile (internal, resolveEdges=false) ====================

func TestIndexFile_NilIndexer(t *testing.T) {
	var idx *Indexer
	err := idx.indexFile(context.Background(), "/any/path.go", false)
	assert.NoError(t, err) // nil-safe
}

func TestIndexFile_NilStore(t *testing.T) {
	idx := &Indexer{}
	err := idx.indexFile(context.Background(), "/any/path.go", false)
	assert.NoError(t, err) // nil-safe
}

func TestIndexFileWithHash_NilIndexer(t *testing.T) {
	var idx *Indexer
	err := idx.indexFileWithHash(context.Background(), "/any/path.go", []byte("x"), "hash", false)
	assert.NoError(t, err)
}

func TestIndexFileWithHash_NilStore(t *testing.T) {
	idx := &Indexer{}
	err := idx.indexFileWithHash(context.Background(), "/any/path.go", []byte("x"), "hash", false)
	assert.NoError(t, err)
}

func TestIndexFileWithHash_GoFile(t *testing.T) {
	idx, st := newTestIndexer(t)

	dir := t.TempDir()
	goFile := filepath.Join(dir, "calc.go")
	src := []byte(`package calc

func Add(a, b int) int {
    return a + b
}
`)
	require.NoError(t, os.WriteFile(goFile, src, 0644))

	err := idx.indexFileWithHash(context.Background(), goFile, src, "testhash", true)
	require.NoError(t, err)

	units, err := st.ListASTUnitsByFile(context.Background(), goFile)
	require.NoError(t, err)
	assert.NotEmpty(t, units)

	// Verify hash was updated
	hash, err := st.GetFileHash(context.Background(), goFile)
	require.NoError(t, err)
	assert.Equal(t, "testhash", hash)
}

// ==================== parseWithTreeSitter edge cases ====================

func TestParseWithTreeSitter_TypeScript_ArrowFunction(t *testing.T) {
	src := []byte(`
const add = (a: number, b: number): number => a + b;

const multiply = (a: number, b: number): number => {
    return a * b;
};
`)
	idx := &Indexer{}
	units, _, err := idx.parseWithTreeSitter(context.Background(), "typescript", "math.ts", src)
	require.NoError(t, err)

	// Arrow functions assigned to const should create variable units
	vars := map[string]bool{}
	for _, u := range units {
		if u.Kind == "variable" {
			vars[u.Name] = true
		}
	}
	assert.True(t, vars["add"], "should find variable add")
	assert.True(t, vars["multiply"], "should find variable multiply")
}

func TestParseWithTreeSitter_TypeScript_TypeAlias(t *testing.T) {
	src := []byte(`
type ID = string;
type User = {
    name: string;
    age: number;
};
`)
	idx := &Indexer{}
	units, _, err := idx.parseWithTreeSitter(context.Background(), "typescript", "types.ts", src)
	require.NoError(t, err)

	types := map[string]bool{}
	for _, u := range units {
		if u.Kind == "type" {
			types[u.Name] = true
		}
	}
	assert.True(t, types["ID"], "should find type alias ID")
	assert.True(t, types["User"], "should find type alias User")
}

func TestParseWithTreeSitter_Java_NewExpression(t *testing.T) {
	src := []byte(`
package com.app;

import java.util.ArrayList;

public class Factory {
    public Object create() {
        return new ArrayList();
    }
}
`)
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "java", "Factory.java", src)
	require.NoError(t, err)
	require.NotEmpty(t, units)

	// Should detect object_creation_expression as call
	foundNew := false
	for _, e := range edges {
		if e.kind == "call" && e.dstName == "ArrayList" {
			foundNew = true
		}
	}
	assert.True(t, foundNew, "should detect new ArrayList() as call edge")
}

func TestParseWithTreeSitter_Java_FieldDeclaration(t *testing.T) {
	src := []byte(`
package com.app;

class Point {
    int x;
    int y;
    String label;
}
`)
	idx := &Indexer{}
	units, _, err := idx.parseWithTreeSitter(context.Background(), "java", "Point.java", src)
	require.NoError(t, err)

	// Should find the class
	classes := map[string]bool{}
	for _, u := range units {
		if u.Kind == "class" {
			classes[u.Name] = true
		}
	}
	assert.True(t, classes["Point"], "should find class Point")

	// Java field_declaration creates variable units for the declarators
	vars := map[string]bool{}
	for _, u := range units {
		if u.Kind == "variable" || u.Kind == "field" {
			vars[u.Name] = true
		}
	}
	// At least some field-related units should exist
	assert.GreaterOrEqual(t, len(vars), 1, "should have field/variable units for class fields")
}

func TestParseWithTreeSitter_CallsInsideMethod(t *testing.T) {
	src := []byte(`
class Service {
    process(): void {
        this.validate();
        this.transform();
        this.save();
    }
}
`)
	idx := &Indexer{}
	units, edges, err := idx.parseWithTreeSitter(context.Background(), "typescript", "svc.ts", src)
	require.NoError(t, err)
	require.NotEmpty(t, units)

	methodFound := false
	for _, u := range units {
		if u.Kind == "method" && u.Name == "process" {
			methodFound = true
		}
	}
	assert.True(t, methodFound)

	// Should have call edges for validate, transform, save
	calls := map[string]bool{}
	for _, e := range edges {
		if e.kind == "call" {
			calls[e.dstName] = true
		}
	}
	assert.True(t, calls["validate"], "should have call to validate")
	assert.True(t, calls["transform"], "should have call to transform")
	assert.True(t, calls["save"], "should have call to save")
}

// ==================== parseGeneric error paths ====================

func TestParseGeneric_NilParser(t *testing.T) {
	// When ts is nil, parseGeneric should handle gracefully
	idx := &Indexer{}
	units, err := idx.parseGeneric(context.Background(), "python", "test.py", []byte("x = 1"))
	// This may panic or return error — we just ensure it doesn't hang
	_ = units
	_ = err
}

// ==================== walkTS edge cases ====================

func TestParseWithTreeSitter_NestedClass(t *testing.T) {
	src := []byte(`
class Outer {
    createInner(): void {
        class Inner {
            value: number;
        }
    }
}
`)
	idx := &Indexer{}
	units, _, err := idx.parseWithTreeSitter(context.Background(), "typescript", "nested.ts", src)
	require.NoError(t, err)

	classes := map[string]bool{}
	for _, u := range units {
		if u.Kind == "class" {
			classes[u.Name] = true
		}
	}
	assert.True(t, classes["Outer"])
	assert.True(t, classes["Inner"], "should find nested class Inner")
}

func TestParseWithTreeSitter_MultipleComments(t *testing.T) {
	src := []byte(`
// First line comment
// Second line comment
function documented(): void {}
`)
	idx := &Indexer{}
	units, _, err := idx.parseWithTreeSitter(context.Background(), "typescript", "comments.ts", src)
	require.NoError(t, err)

	for _, u := range units {
		if u.Kind == "function" && u.Name == "documented" {
			assert.Contains(t, u.Doc, "First line comment")
			assert.Contains(t, u.Doc, "Second line comment")
		}
	}
}
