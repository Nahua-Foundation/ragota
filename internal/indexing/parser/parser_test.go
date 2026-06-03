package parser

// End-to-end тесты для Parser.Parse / ParseChunks / ParseAll поверх
// tree-sitter (CGO). Проверяются ключевые языки: Go (function/method/type/
// const/var, лидирующий комментарий, импорты) и Python (function/class с
// docstring, методы класса как parent=ClassName). Тесты не зависят от сети
// или внешних сервисов.

import (
	"context"
	"strings"
	"testing"
)

func mustParse(t *testing.T, lang, path, src string) ([]Symbol, []Symbol) {
	t.Helper()
	p := New()
	syms, chunks, err := p.ParseAll(context.Background(), lang, path, []byte(src), 4096)
	if err != nil {
		t.Fatalf("ParseAll(%s): %v", lang, err)
	}
	return syms, chunks
}

func findSym(syms []Symbol, name string) *Symbol {
	for i := range syms {
		if syms[i].Name == name {
			return &syms[i]
		}
	}
	return nil
}

func TestParse_UnknownLanguage(t *testing.T) {
	p := New()
	syms, chunks, err := p.ParseAll(context.Background(), "cobol", "x.cbl", []byte("PROGRAM."), 1024)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if syms != nil || chunks != nil {
		t.Errorf("expected nil/nil for unknown lang, got %v / %v", syms, chunks)
	}
}

func TestParse_Go(t *testing.T) {
	src := `package demo

import (
	"fmt"
	"strings"
)

// Greeting returns hello.
func Greeting(name string) string {
	return fmt.Sprintf("hello %s", strings.ToUpper(name))
}

type User struct {
	Name string
}

// Hello on User.
func (u *User) Hello() string { return "hi " + u.Name }

	const Pi = 3.14
	var Counter = 0

	const (
		Max = 100
		Min = 0
	)
	var (
		Total = 0
		Count = 10
	)
`
	syms, chunks := mustParse(t, "go", "demo.go", src)

	// Symbols presence
	for _, want := range []string{"Greeting", "User", "Hello", "Pi", "Counter", "Max", "Min", "Total", "Count"} {
		if findSym(syms, want) == nil {
			t.Errorf("symbol %q not found; got %+v", want, syms)
		}
	}

	// Kind correctness
	if s := findSym(syms, "Greeting"); s != nil && s.Kind != "function" {
		t.Errorf("Greeting.Kind=%q, want function", s.Kind)
	}
	if s := findSym(syms, "Hello"); s != nil && s.Kind != "method" {
		t.Errorf("Hello.Kind=%q, want method", s.Kind)
	}

	// Doc-comment пристёгнут к Greeting
	if s := findSym(syms, "Greeting"); s != nil && !strings.Contains(s.Doc, "Greeting returns hello") {
		t.Errorf("Greeting.Doc missing doc-comment: %q", s.Doc)
	}

	// Signature первая строка
	if s := findSym(syms, "Greeting"); s != nil && !strings.Contains(s.Signature, "func Greeting") {
		t.Errorf("Greeting.Signature=%q", s.Signature)
	}

	// Signature для констант и переменных
	if s := findSym(syms, "Pi"); s != nil && !strings.Contains(s.Signature, "Pi = 3.14") {
		t.Errorf("Pi.Signature=%q", s.Signature)
	}
	if s := findSym(syms, "Max"); s != nil && !strings.Contains(s.Signature, "Max = 100") {
		t.Errorf("Max.Signature=%q", s.Signature)
	}

	// Чанки покрывают файл и содержат импорты
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	gotImports := false
	for _, c := range chunks {
		for _, imp := range c.Imports {
			if strings.Contains(imp, "fmt") {
				gotImports = true
			}
		}
	}
	if !gotImports {
		t.Errorf("expected fmt import in chunks, got chunks=%d", len(chunks))
	}
}

func TestParse_PythonDocstringAndClass(t *testing.T) {
	src := `import os

def hello(name):
    """Say hi."""
    return "hi " + name

class Greeter:
    """A greeter."""

    def greet(self, name):
        return "hi " + name
`
	syms, _ := mustParse(t, "python", "demo.py", src)

	hello := findSym(syms, "hello")
	if hello == nil {
		t.Fatalf("hello not found; syms=%+v", syms)
	}
	if hello.Kind != "function" {
		t.Errorf("hello.Kind=%q", hello.Kind)
	}
	if !strings.Contains(hello.Doc, "Say hi") {
		t.Errorf("hello.Doc=%q (expected docstring)", hello.Doc)
	}

	cls := findSym(syms, "Greeter")
	if cls == nil || cls.Kind != "class" {
		t.Fatalf("Greeter not found or wrong kind: %+v", cls)
	}

	greet := findSym(syms, "greet")
	if greet == nil {
		t.Fatalf("greet not found")
	}
	if greet.Parent != "Greeter" {
		t.Errorf("greet.Parent=%q, want Greeter", greet.Parent)
	}
}

func TestParseChunks_CoversAllFile(t *testing.T) {
	src := `package demo

func A() {}
func B() {}
func C() {}
`
	p := New()
	chunks := p.ParseChunks(context.Background(), "go", "demo.go", []byte(src), 4096)
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	// Все байты файла должны быть покрыты объединением чанков (с учётом
	// возможного перекрытия — проверяем хотя бы что начало и конец достижимы).
	minStart, maxEnd := chunks[0].StartByte, chunks[0].EndByte
	for _, c := range chunks {
		if c.StartByte < minStart {
			minStart = c.StartByte
		}
		if c.EndByte > maxEnd {
			maxEnd = c.EndByte
		}
	}
	if minStart > 0 {
		t.Errorf("chunks start at %d, expected 0", minStart)
	}
	if maxEnd < len(src)-1 {
		t.Errorf("chunks end at %d, expected >= %d", maxEnd, len(src)-1)
	}
}

func TestParse_SyntaxErrorDoesNotPanic(t *testing.T) {
	// Битый Go — tree-sitter всё равно строит дерево с ERROR-узлами;
	// важно, что Parse не паникует и возвращает то, что смог.
	src := `package demo
func A( {
`
	_, _, err := New().ParseAll(context.Background(), "go", "x.go", []byte(src), 1024)
	if err != nil {
		t.Errorf("unexpected err on broken source: %v", err)
	}
}
