package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestImplementation_JS(t *testing.T) {
	if _, err := exec.LookPath("typescript-language-server"); err != nil {
		t.Skip("typescript-language-server not in PATH")
	}

	cwd, _ := os.Getwd()
	// Переходим в корень проекта, чтобы пути были корректными
	root := filepath.Join(cwd, "../..")
	testProjectRoot, _ := filepath.Abs(filepath.Join(root, "tests/testprojects/js"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec := ServerSpec{
		Language: "typescript",
		Command:  "typescript-language-server",
		Args:     []string{"--stdio"},
	}

	client, err := Start(ctx, spec, testProjectRoot)
	if err != nil {
		t.Fatalf("Start JS LSP: %v", err)
	}
	defer client.Close()

	baseFile := filepath.Join(testProjectRoot, "lib/base.js")
	derivedFile := filepath.Join(testProjectRoot, "lib/derived.js")

	contentBase, _ := os.ReadFile(baseFile)
	if err := client.DidOpen(context.Background(), baseFile, "javascript", string(contentBase)); err != nil {
		t.Fatalf("DidOpen base: %v", err)
	}

	contentDerived, _ := os.ReadFile(derivedFile)
	if err := client.DidOpen(context.Background(), derivedFile, "javascript", string(contentDerived)); err != nil {
		t.Fatalf("DidOpen derived: %v", err)
	}

	// Ждем индексации
	time.Sleep(2 * time.Second)

	// В lib/base.js метод start() на 2-й строке (0-indexed line=1)
	// class Base {
	//     start() {}
	// }
	locs, err := client.Implementation(ctx, baseFile, 1, 4)
	if err != nil {
		t.Errorf("Implementation JS ERROR: %v", err)
	}

	t.Logf("Implementation JS RESULT: %v", locs)
	// Ожидаем, что найдет реализацию в derived.js
}

func TestImplementation_Python(t *testing.T) {
	if _, err := exec.LookPath("pyright-langserver"); err != nil {
		t.Skip("pyright-langserver not in PATH")
	}

	// cwd, _ := os.Getwd()
	// root := filepath.Join(cwd, "../..") // unused

	// Создаем временный питон-проект
	testProjectRoot := t.TempDir()
	pyFile := filepath.Join(testProjectRoot, "main.py")
	content := `
class Base:
    def start(self):
        pass

class Derived(Base):
    def start(self):
        print("derived")
`
	os.WriteFile(pyFile, []byte(content), 0644)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec := ServerSpec{
		Language: "python",
		Command:  "pyright-langserver",
		Args:     []string{"--stdio"},
	}

	client, err := Start(ctx, spec, testProjectRoot)
	if err != nil {
		t.Fatalf("Start Python LSP: %v", err)
	}
	defer client.Close()

	if err := client.DidOpen(context.Background(), pyFile, "python", content); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}

	time.Sleep(2 * time.Second)

	// Base.start на line=2, char=8
	locs, err := client.Implementation(ctx, pyFile, 2, 8)
	if err != nil {
		t.Errorf("Implementation Python ERROR: %v", err)
	}

	if len(locs) != 0 {
		t.Errorf("Implementation Python: expected 0 locations, got %d", len(locs))
	}

	t.Logf("Implementation Python RESULT: %v", locs)
}

func TestLSP_Go(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not in PATH")
	}

	cwd, _ := os.Getwd()
	root := filepath.Join(cwd, "../..")
	testProjectRoot, _ := filepath.Abs(filepath.Join(root, "tests/testprojects/go"))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	spec := ServerSpec{
		Language: "go",
		Command:  "gopls",
	}

	client, err := Start(ctx, spec, testProjectRoot)
	if err != nil {
		t.Fatalf("Start Go LSP: %v", err)
	}
	defer client.Close()

	mainFile := filepath.Join(testProjectRoot, "main.go")
	content, _ := os.ReadFile(mainFile)
	if err := client.DidOpen(context.Background(), mainFile, "go", string(content)); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}

	// Definition
	// Equal call at line 8, char 4 (e.Equal)
	locs, err := client.Definition(ctx, mainFile, 7, 4)
	if err != nil {
		t.Errorf("Definition Go ERROR: %v", err)
	}
	t.Logf("Definition Go RESULT: %v", locs)

	// Hover
	hover, err := client.Hover(ctx, mainFile, 7, 4)
	if err != nil {
		t.Errorf("Hover Go ERROR: %v", err)
	}
	t.Logf("Hover Go RESULT: %s", hover)
}

func TestLSP_Java(t *testing.T) {
	if _, err := exec.LookPath("jdtls"); err != nil {
		t.Skip("jdtls not in PATH")
	}
	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("java not in PATH")
	}

	cwd, _ := os.Getwd()
	root := filepath.Join(cwd, "../..")
	testProjectRoot, _ := filepath.Abs(filepath.Join(root, "tests/testprojects/java"))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	spec := ServerSpec{
		Language: "java",
		Command:  "jdtls",
		Args: []string{
			"-data", t.TempDir(),
		},
	}

	client, err := Start(ctx, spec, testProjectRoot)
	if err != nil {
		t.Fatalf("Start Java LSP: %v", err)
	}
	defer client.Close()

	mainFile := filepath.Join(testProjectRoot, "Main.java")
	content, _ := os.ReadFile(mainFile)
	if err := client.DidOpen(context.Background(), mainFile, "java", string(content)); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}

	// Definition
	// execute() call at line 6, char 11 (s.execute)
	locs, err := client.Definition(ctx, mainFile, 5, 11)
	if err != nil {
		t.Errorf("Definition Java ERROR: %v", err)
	}
	t.Logf("Definition Java RESULT: %v", locs)

	// Hover
	hover, err := client.Hover(ctx, mainFile, 5, 11)
	if err != nil {
		t.Errorf("Hover Java ERROR: %v", err)
	}
	t.Logf("Hover Java RESULT: %s", hover)
}
