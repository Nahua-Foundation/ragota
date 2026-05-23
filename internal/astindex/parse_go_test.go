package astindex

// Unit-тесты Go-специфичного экстрактора parseGo и чистых helper'ов из
// util.go. БД и сеть не используются — Indexer вызывается напрямую с
// нулевым cfg/st (parseGo не обращается ни к одному полю Indexer).

import (
	"strings"
	"testing"
)

const sampleGo = `// Package demo is a sample package.
package demo

import (
	"fmt"
	"strings"
)

// Greeter greets people.
type Greeter struct {
	Name string
}

// Embedder embeds Greeter.
type Embedder struct {
	Greeter
}

// Talker is an embedded-interface example.
type Talker interface {
	io.Reader
	Talk() string
}

const Pi = 3.14
var Version = "1.0"

// Hello prints a greeting.
func Hello(name string) string {
	s := fmt.Sprintf("hi %s", name)
	return strings.ToUpper(s)
}

// Greet is a method on *Greeter.
func (g *Greeter) Greet() string {
	return "hello " + g.Name
}
`

func findUnit(units []unitLike, kind, name string) *unitLike {
	for i := range units {
		if units[i].Kind == kind && units[i].Name == name {
			return &units[i]
		}
	}
	return nil
}

// unitLike — мини-проекция store.ASTUnit для удобства проверок без импорта store.
type unitLike struct {
	Kind, Name, Qualified, Signature, Doc string
	StartLine, EndLine                    int
}

func TestParseGo_ExtractsUnitsAndEdges(t *testing.T) {
	idx := &Indexer{}
	units, edges, err := idx.parseGo("demo.go", []byte(sampleGo))
	if err != nil {
		t.Fatalf("parseGo: %v", err)
	}
	if len(units) == 0 {
		t.Fatal("parseGo: 0 units")
	}

	// Проектируем для удобства поиска.
	proj := make([]unitLike, len(units))
	for i, u := range units {
		proj[i] = unitLike{Kind: u.Kind, Name: u.Name, Qualified: u.Qualified, Signature: u.Signature, Doc: u.Doc, StartLine: u.StartLine, EndLine: u.EndLine}
	}

	// module unit.
	mod := findUnit(proj, "module", "demo")
	if mod == nil || mod.Qualified != "demo" || !strings.Contains(mod.Signature, "package demo") {
		t.Errorf("module unit malformed: %+v", mod)
	}

	// function Hello.
	hello := findUnit(proj, "function", "Hello")
	if hello == nil {
		t.Fatalf("function Hello not found; got: %+v", proj)
	}
	if hello.Qualified != "demo.Hello" {
		t.Errorf("Hello.Qualified = %q; want demo.Hello", hello.Qualified)
	}
	if !strings.Contains(hello.Signature, "func Hello(name string) string") {
		t.Errorf("Hello.Signature = %q", hello.Signature)
	}
	if !strings.Contains(hello.Doc, "Hello prints a greeting") {
		t.Errorf("Hello.Doc missing: %q", hello.Doc)
	}

	// method Greet — qualified должен включать receiver type.
	greet := findUnit(proj, "method", "Greet")
	if greet == nil || greet.Qualified != "demo.Greeter.Greet" {
		t.Errorf("method Greet: %+v", greet)
	}

	// struct/interface.
	if findUnit(proj, "struct", "Greeter") == nil {
		t.Errorf("struct Greeter not found")
	}
	if findUnit(proj, "interface", "Talker") == nil {
		t.Errorf("interface Talker not found")
	}

	// const / var.
	if findUnit(proj, "constant", "Pi") == nil {
		t.Errorf("constant Pi not found")
	}
	if findUnit(proj, "variable", "Version") == nil {
		t.Errorf("variable Version not found")
	}

	// Edges: imports + calls + embedded reference/extends.
	var importPaths, calls, refs, exts []string
	for _, e := range edges {
		switch e.kind {
		case "import":
			importPaths = append(importPaths, e.dstName)
		case "call":
			calls = append(calls, e.dstName)
		case "reference":
			refs = append(refs, e.dstName)
		case "extends":
			exts = append(exts, e.dstName)
		}
	}
	wantImports := map[string]bool{"fmt": false, "strings": false}
	for _, p := range importPaths {
		if _, ok := wantImports[p]; ok {
			wantImports[p] = true
		}
	}
	for k, v := range wantImports {
		if !v {
			t.Errorf("missing import edge: %s (got %v)", k, importPaths)
		}
	}
	// Hello вызывает fmt.Sprintf и strings.ToUpper.
	if !containsAny(calls, "fmt.Sprintf") || !containsAny(calls, "strings.ToUpper") {
		t.Errorf("call edges missing fmt.Sprintf/strings.ToUpper: %v", calls)
	}
	// Embedder содержит embedded Greeter → reference.
	if !containsAny(refs, "Greeter") {
		t.Errorf("reference edge to Greeter missing: %v", refs)
	}
	// Talker встраивает io.Reader → extends.
	if !containsAny(exts, "io.Reader") {
		t.Errorf("extends edge to io.Reader missing: %v", exts)
	}
}

func TestParseGo_SyntaxError(t *testing.T) {
	idx := &Indexer{}
	_, _, err := idx.parseGo("bad.go", []byte("package demo\nfunc ("))
	if err == nil {
		t.Fatal("parseGo: expected error on syntactically invalid file")
	}
}

func TestParseGo_EmptyPackage(t *testing.T) {
	idx := &Indexer{}
	units, edges, err := idx.parseGo("empty.go", []byte("package empty\n"))
	if err != nil {
		t.Fatalf("parseGo: %v", err)
	}
	if len(units) != 1 || units[0].Kind != "module" || units[0].Name != "empty" {
		t.Errorf("expected single module unit, got: %+v", units)
	}
	if len(edges) != 0 {
		t.Errorf("expected no edges, got: %+v", edges)
	}
}

func containsAny(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// ---- util.go ----

func TestDetectLang(t *testing.T) {
	cases := map[string]string{
		"a.go":      "go",
		"a.ts":      "typescript",
		"a.tsx":     "typescript",
		"a.js":      "javascript",
		"a.mjs":     "javascript",
		"x.py":      "python",
		"X.JAVA":    "java",
		"README.md": "",
		"noext":     "",
	}
	for in, want := range cases {
		if got := detectLang(in); got != want {
			t.Errorf("detectLang(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("hello\nworld"); got != "hello" {
		t.Errorf("firstLine: %q", got)
	}
	if got := firstLine("   one-line   "); got != "one-line" {
		t.Errorf("firstLine trim: %q", got)
	}
	long := strings.Repeat("x", 250)
	got := firstLine(long)
	if len(got) != 203 || !strings.HasSuffix(got, "...") {
		t.Errorf("firstLine long: len=%d suffix=%q", len(got), got[len(got)-3:])
	}
}

func TestHashBytes_Stable(t *testing.T) {
	a := hashBytes([]byte("abc"))
	b := hashBytes([]byte("abc"))
	c := hashBytes([]byte("abd"))
	if a != b {
		t.Errorf("hashBytes not deterministic: %s vs %s", a, b)
	}
	if a == c {
		t.Errorf("hashBytes collision for different inputs")
	}
	if len(a) != 40 {
		t.Errorf("hashBytes len = %d; want 40 (sha1 hex)", len(a))
	}
}
