package parser

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Parse: TypeScript
// ---------------------------------------------------------------------------

func TestParse_TypeScript_ClassAndFunction(t *testing.T) {
	src := `import { Foo } from './foo';

// Greeter is a class.
class Greeter {
  name: string;

  greet(who: string): string {
    return "hello " + who;
  }
}

// doWork performs work.
function doWork(x: number): number {
  return x * 2;
}

interface Shape {
  area(): number;
}

type Callback = (err: Error | null) => void;

enum Color { Red, Green, Blue }
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "typescript", "demo.ts", []byte(src), 4096)
	require.NoError(t, err)

	find := func(name string) *Symbol {
		for i := range syms {
			if syms[i].Name == name {
				return &syms[i]
			}
		}
		return nil
	}

	// Class
	cls := find("Greeter")
	require.NotNil(t, cls, "Greeter not found")
	assert.Equal(t, "class", cls.Kind)
	assert.Contains(t, cls.Doc, "Greeter is a class")

	// Method inside class
	greet := find("greet")
	require.NotNil(t, greet, "greet not found")
	assert.Equal(t, "method", greet.Kind)
	assert.Equal(t, "Greeter", greet.Parent)

	// Function
	fn := find("doWork")
	require.NotNil(t, fn, "doWork not found")
	assert.Equal(t, "function", fn.Kind)
	assert.Contains(t, fn.Doc, "doWork performs work")

	// Interface
	iface := find("Shape")
	require.NotNil(t, iface, "Shape not found")
	assert.Equal(t, "interface", iface.Kind)

	// Type alias
	ta := find("Callback")
	require.NotNil(t, ta, "Callback not found")
	assert.Equal(t, "type", ta.Kind)

	// Enum
	en := find("Color")
	require.NotNil(t, en, "Color not found")
	assert.Equal(t, "enum", en.Kind)
}

// ---------------------------------------------------------------------------
// Parse: TSX
// ---------------------------------------------------------------------------

func TestParse_TSX(t *testing.T) {
	src := `function Hello(props: { name: string }) {
  return <div>Hello {props.name}</div>;
}
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "typescript", "comp.tsx", []byte(src), 4096)
	require.NoError(t, err)
	found := false
	for _, s := range syms {
		if s.Name == "Hello" {
			found = true
			assert.Equal(t, "function", s.Kind)
		}
	}
	assert.True(t, found, "Hello function not found in TSX")
}

// ---------------------------------------------------------------------------
// Parse: JavaScript
// ---------------------------------------------------------------------------

func TestParse_JavaScript_FunctionDeclaration(t *testing.T) {
	src := `function add(a, b) {
  return a + b;
}

class Animal {
  constructor(name) {
    this.name = name;
  }
  speak() {
    return this.name + " makes a noise.";
  }
}
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "javascript", "demo.js", []byte(src), 4096)
	require.NoError(t, err)

	find := func(name string) *Symbol {
		for i := range syms {
			if syms[i].Name == name {
				return &syms[i]
			}
		}
		return nil
	}

	fn := find("add")
	require.NotNil(t, fn, "add not found")
	assert.Equal(t, "function", fn.Kind)

	cls := find("Animal")
	require.NotNil(t, cls, "Animal not found")
	assert.Equal(t, "class", cls.Kind)

	// Methods inside JS class
	ctor := find("constructor")
	if ctor != nil {
		assert.Equal(t, "Animal", ctor.Parent)
	}
	speak := find("speak")
	require.NotNil(t, speak, "speak not found")
	assert.Equal(t, "method", speak.Kind)
	assert.Equal(t, "Animal", speak.Parent)
}

// ---------------------------------------------------------------------------
// Parse: JSX
// ---------------------------------------------------------------------------

func TestParse_JSX(t *testing.T) {
	src := `function App() {
  return <div className="app">Hello</div>;
}
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "javascript", "app.jsx", []byte(src), 4096)
	require.NoError(t, err)
	found := false
	for _, s := range syms {
		if s.Name == "App" && s.Kind == "function" {
			found = true
		}
	}
	assert.True(t, found, "App function not found in JSX")
}

// ---------------------------------------------------------------------------
// Parse: Java
// ---------------------------------------------------------------------------

func TestParse_Java_ClassAndMethod(t *testing.T) {
	src := `import java.util.List;

// Main application class.
public class App {
    public static void main(String[] args) {
        System.out.println("hello");
    }

    int compute(int x) {
        return x * 2;
    }
}

interface Runnable {
    void run();
}

enum Status {
    ACTIVE, INACTIVE
}
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "java", "App.java", []byte(src), 4096)
	require.NoError(t, err)

	find := func(name string) *Symbol {
		for i := range syms {
			if syms[i].Name == name {
				return &syms[i]
			}
		}
		return nil
	}

	cls := find("App")
	require.NotNil(t, cls, "App not found")
	assert.Equal(t, "class", cls.Kind)
	assert.Contains(t, cls.Doc, "Main application class")

	main := find("main")
	require.NotNil(t, main, "main not found")
	assert.Equal(t, "method", main.Kind)
	assert.Equal(t, "App", main.Parent)

	compute := find("compute")
	require.NotNil(t, compute, "compute not found")
	assert.Equal(t, "method", compute.Kind)

	iface := find("Runnable")
	require.NotNil(t, iface, "Runnable not found")
	assert.Equal(t, "interface", iface.Kind)

	en := find("Status")
	require.NotNil(t, en, "Status not found")
	assert.Equal(t, "enum", en.Kind)
}

// ---------------------------------------------------------------------------
// Import extraction
// ---------------------------------------------------------------------------

func TestParse_GoImports(t *testing.T) {
	src := `package main

import "fmt"
import "strings"

func main() { fmt.Println(strings.ToUpper("hello")) }
`
	p := New()
	syms, chunks, err := p.ParseAll(context.Background(), "go", "main.go", []byte(src), 4096)
	require.NoError(t, err)
	assert.NotEmpty(t, syms)

	// Imports should be attached to chunks
	hasImport := false
	for _, c := range chunks {
		for _, imp := range c.Imports {
			if strings.Contains(imp, "fmt") {
				hasImport = true
			}
		}
	}
	assert.True(t, hasImport, "expected fmt in chunk imports")
}

func TestParse_PythonImports(t *testing.T) {
	src := `import os
from sys import argv

def main():
    pass
`
	p := New()
	_, chunks, err := p.ParseAll(context.Background(), "python", "main.py", []byte(src), 4096)
	require.NoError(t, err)

	hasOS := false
	hasSys := false
	for _, c := range chunks {
		for _, imp := range c.Imports {
			if strings.Contains(imp, "os") {
				hasOS = true
			}
			if strings.Contains(imp, "sys") {
				hasSys = true
			}
		}
	}
	assert.True(t, hasOS, "expected 'import os' in chunks")
	assert.True(t, hasSys, "expected 'from sys' in chunks")
}

func TestParse_JavaImports(t *testing.T) {
	src := `import java.util.List;
import java.util.Map;

class Foo {
    List<String> items;
}
`
	p := New()
	_, chunks, err := p.ParseAll(context.Background(), "java", "Foo.java", []byte(src), 4096)
	require.NoError(t, err)

	hasList := false
	for _, c := range chunks {
		for _, imp := range c.Imports {
			if strings.Contains(imp, "java.util.List") {
				hasList = true
			}
		}
	}
	assert.True(t, hasList, "expected java.util.List import")
}

func TestParse_TSImports(t *testing.T) {
	src := `import { Component } from 'react';
import express from 'express';

function App() {}
`
	p := New()
	_, chunks, err := p.ParseAll(context.Background(), "typescript", "app.ts", []byte(src), 4096)
	require.NoError(t, err)

	hasReact := false
	for _, c := range chunks {
		for _, imp := range c.Imports {
			if strings.Contains(imp, "react") {
				hasReact = true
			}
		}
	}
	assert.True(t, hasReact, "expected react import")
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestParse_EmptySource(t *testing.T) {
	p := New()
	for _, lang := range SupportedLanguages() {
		ext := ".go"
		switch lang {
		case "python":
			ext = ".py"
		case "java":
			ext = ".java"
		case "typescript":
			ext = ".ts"
		case "javascript":
			ext = ".js"
		}
		syms, chunks, err := p.ParseAll(context.Background(), lang, "empty"+ext, []byte(""), 4096)
		assert.NoError(t, err, "lang=%s", lang)
		assert.Empty(t, syms, "lang=%s should have no symbols", lang)
		// chunks might contain one empty-ish chunk from root — just no panic
		_ = chunks
	}
}

func TestParse_OnlyComments(t *testing.T) {
	src := `// just a comment
// another comment
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "go", "comments.go", []byte(src), 4096)
	require.NoError(t, err)
	// No actual declarations → no symbols (comments alone aren't symbols)
	assert.Empty(t, syms)
}

func TestParse_UnicodeSource(t *testing.T) {
	src := `package main

// Привет мир
func Привет() string {
	return "мир"
}
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "go", "unicode.go", []byte(src), 4096)
	require.NoError(t, err)
	found := false
	for _, s := range syms {
		if s.Name == "Привет" {
			found = true
			assert.Contains(t, s.Doc, "Привет мир")
		}
	}
	assert.True(t, found, "Unicode function name not found")
}

func TestParse_DeeplyNestedFunctions(t *testing.T) {
	// Go doesn't allow nested funcs, but TS does
	src := `function outer() {
  function inner() {
    function deepest() {
      return 42;
    }
    return deepest();
  }
  return inner();
}
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "typescript", "nested.ts", []byte(src), 4096)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, s := range syms {
		names[s.Name] = true
	}
	assert.True(t, names["outer"], "outer not found")
	assert.True(t, names["inner"], "inner not found")
	assert.True(t, names["deepest"], "deepest not found")
}

func TestParse_ChunksMaxBytesSmall(t *testing.T) {
	// Very small maxBytes — should still produce chunks without panic
	src := `package main

func LongFunction() string {
    return "this is a very long function body that exceeds small max bytes limit"
}
`
	p := New()
	syms, chunks, err := p.ParseAll(context.Background(), "go", "big.go", []byte(src), 50)
	require.NoError(t, err)
	assert.NotEmpty(t, syms)
	assert.NotEmpty(t, chunks)
}

func TestParse_ConstAndVarSpecs(t *testing.T) {
	src := `package demo

const (
	A = 1
	B = 2
)

var (
	X = "hello"
	Y = "world"
)

const SingleConst = true
var SingleVar = 42
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "go", "spec.go", []byte(src), 4096)
	require.NoError(t, err)

	find := func(name string) *Symbol {
		for i := range syms {
			if syms[i].Name == name {
				return &syms[i]
			}
		}
		return nil
	}

	for _, name := range []string{"A", "B", "X", "Y", "SingleConst", "SingleVar"} {
		s := find(name)
		require.NotNil(t, s, "%s not found", name)
		if strings.HasPrefix(name, "A") || strings.HasPrefix(name, "B") || name == "SingleConst" {
			assert.Equal(t, "const", s.Kind, name)
		} else {
			assert.Equal(t, "var", s.Kind, name)
		}
	}
}

// ---------------------------------------------------------------------------
// isCommentNode (internal helper)
// ---------------------------------------------------------------------------

func TestIsCommentNode(t *testing.T) {
	assert.True(t, isCommentNode("comment"))
	assert.True(t, isCommentNode("line_comment"))
	assert.True(t, isCommentNode("block_comment"))
	assert.True(t, isCommentNode("documentation_comment"))
	assert.False(t, isCommentNode("function_declaration"))
	assert.False(t, isCommentNode(""))
	assert.False(t, isCommentNode("string"))
}

// ---------------------------------------------------------------------------
// canonicalKind edge cases
// ---------------------------------------------------------------------------

func TestCanonicalKind_AllMappings(t *testing.T) {
	tests := map[string]string{
		"function_declaration":    "function",
		"method_declaration":      "method",
		"type_declaration":        "type",
		"type_spec":               "type",
		"const_declaration":       "const",
		"const_spec":              "const",
		"var_declaration":         "var",
		"var_spec":                "var",
		"function_definition":     "function",
		"class_definition":        "class",
		"class_declaration":       "class",
		"interface_declaration":   "interface",
		"enum_declaration":        "enum",
		"method_definition":       "method",
		"type_alias_declaration":  "type",
		// Non-matches
		"if_statement":       "",
		"for_statement":      "",
		"return_statement":   "",
		"identifier":         "",
		"string_literal":     "",
		"":                   "",
	}
	for input, want := range tests {
		assert.Equal(t, want, canonicalKind(input), "canonicalKind(%q)", input)
	}
}

// ---------------------------------------------------------------------------
// lineForByte edge cases
// ---------------------------------------------------------------------------

func TestLineForByte_EmptySource(t *testing.T) {
	assert.Equal(t, 1, lineForByte([]byte{}, 0))
}

func TestLineForByte_OnlyNewlines(t *testing.T) {
	src := []byte("\n\n\n")
	assert.Equal(t, 1, lineForByte(src, 0))
	assert.Equal(t, 2, lineForByte(src, 1))
	assert.Equal(t, 3, lineForByte(src, 2))
	assert.Equal(t, 4, lineForByte(src, 3)) // past end
}

func TestLineForByte_ExactlyAtEnd(t *testing.T) {
	src := []byte("abc")
	assert.Equal(t, 1, lineForByte(src, 3)) // clamped to len
}

// ---------------------------------------------------------------------------
// indexByte edge cases
// ---------------------------------------------------------------------------

func TestIndexByte_EmptySlice(t *testing.T) {
	assert.Equal(t, -1, indexByte([]byte{}, 'a'))
}

func TestIndexByte_FirstByte(t *testing.T) {
	assert.Equal(t, 0, indexByte([]byte("abc"), 'a'))
}

func TestIndexByte_LastByte(t *testing.T) {
	assert.Equal(t, 2, indexByte([]byte("abc"), 'c'))
}

func TestIndexByte_NullByte(t *testing.T) {
	assert.Equal(t, 2, indexByte([]byte("ab\x00cd"), 0))
}

// ---------------------------------------------------------------------------
// firstLine edge case via parse (large signature truncated to 200 chars)
// ---------------------------------------------------------------------------

func TestParse_LongSignature(t *testing.T) {
	// Generate a function with a very long parameter list
	params := make([]string, 50)
	for i := range params {
		params[i] = "param" + strings.Repeat("x", 5) + string(rune('A'+i%26)) + " int"
	}
	src := "package main\n\nfunc VeryLongFunc(" + strings.Join(params, ", ") + ") {}\n"

	p := New()
	syms, _, err := p.ParseAll(context.Background(), "go", "long.go", []byte(src), 8192)
	require.NoError(t, err)
	found := false
	for _, s := range syms {
		if s.Name == "VeryLongFunc" {
			found = true
			// firstLine truncates to 200 chars + "..."
			assert.LessOrEqual(t, len(s.Signature), 204) // 200 + "..."
			if len(s.Signature) > 200 {
				assert.True(t, strings.HasSuffix(s.Signature, "..."))
			}
		}
	}
	assert.True(t, found, "VeryLongFunc not found")
}

// ---------------------------------------------------------------------------
// Parse vs ParseChunks consistency
// ---------------------------------------------------------------------------

func TestParseAndParseChunks_Consistent(t *testing.T) {
	src := `package main

import "fmt"

func A() { fmt.Println("A") }
func B() { fmt.Println("B") }
`
	p := New()
	ctx := context.Background()

	syms, err := p.Parse(ctx, "go", "demo.go", []byte(src))
	require.NoError(t, err)

	chunks := p.ParseChunks(ctx, "go", "demo.go", []byte(src), 4096)
	assert.NotEmpty(t, chunks)

	// ParseAll should give same symbols as Parse
	syms2, chunks2, err := p.ParseAll(ctx, "go", "demo.go", []byte(src), 4096)
	require.NoError(t, err)
	assert.Equal(t, len(syms), len(syms2))
	assert.Equal(t, len(chunks), len(chunks2))
}

// ---------------------------------------------------------------------------
// Python docstring extraction
// ---------------------------------------------------------------------------

func TestParse_PythonMultiLineDocstring(t *testing.T) {
	src := `def complex_func(x, y):
    """
    Compute the sum of x and y.

    Args:
        x: first number
        y: second number

    Returns:
        The sum.
    """
    return x + y
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "python", "math.py", []byte(src), 4096)
	require.NoError(t, err)

	found := false
	for _, s := range syms {
		if s.Name == "complex_func" {
			found = true
			assert.Contains(t, s.Doc, "Compute the sum")
			assert.Contains(t, s.Doc, "Args:")
		}
	}
	assert.True(t, found, "complex_func not found")
}

func TestParse_PythonClassWithDocstring(t *testing.T) {
	src := `class Calculator:
    """A simple calculator."""

    def add(self, a, b):
        return a + b

    def sub(self, a, b):
        return a - b
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "python", "calc.py", []byte(src), 4096)
	require.NoError(t, err)

	find := func(name string) *Symbol {
		for i := range syms {
			if syms[i].Name == name {
				return &syms[i]
			}
		}
		return nil
	}

	cls := find("Calculator")
	require.NotNil(t, cls)
	assert.Equal(t, "class", cls.Kind)
	assert.Contains(t, cls.Doc, "A simple calculator")

	add := find("add")
	require.NotNil(t, add)
	// Python function_definition is always "function" in tree-sitter (no separate method kind)
	assert.Equal(t, "function", add.Kind)
	assert.Equal(t, "Calculator", add.Parent)

	sub := find("sub")
	require.NotNil(t, sub)
	assert.Equal(t, "Calculator", sub.Parent)
}

// ---------------------------------------------------------------------------
// Java: comment before class
// ---------------------------------------------------------------------------

func TestParse_JavaBlockComment(t *testing.T) {
	src := `/**
 * Utility class for string operations.
 */
class StringUtils {
    static String reverse(String s) {
        return new StringBuilder(s).reverse().toString();
    }
}
`
	p := New()
	syms, _, err := p.ParseAll(context.Background(), "java", "StringUtils.java", []byte(src), 4096)
	require.NoError(t, err)

	found := false
	for _, s := range syms {
		if s.Name == "StringUtils" {
			found = true
			assert.Equal(t, "class", s.Kind)
			// Block comment should be captured as doc
			if s.Doc != "" {
				assert.Contains(t, s.Doc, "Utility class")
			}
		}
	}
	assert.True(t, found, "StringUtils not found")
}
