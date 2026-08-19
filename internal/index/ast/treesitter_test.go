package ast

import (
	"sync"
	"testing"
)

func TestTreeSitterParserConcurrent(t *testing.T) {
	p := NewTreeSitterParser("go")
	content := "package main\n\nfunc Add(a, b int) int { return a + b }\n"

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, _, err := p.Parse("a.go", content); err != nil {
					t.Errorf("Parse: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestParseExtractsFunctionsMethodsTypes(t *testing.T) {
	p := NewTreeSitterParser("go")
	src := "package main\n\n" +
		"type Person struct{ Name string }\n\n" +
		"func (p *Person) Greet() string { return p.Name }\n\n" +
		"func Add(a, b int) int { return a + b }\n"

	units, _, err := p.Parse("x.go", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	kind := map[string]string{}
	for _, u := range units {
		kind[u.Name] = u.Kind
		if u.Hash == "" {
			t.Errorf("unit %q has empty Hash", u.Name)
		}
	}
	if kind["Add"] != "function" {
		t.Errorf("Add kind = %q, want function", kind["Add"])
	}
	if kind["Greet"] != "method" {
		t.Errorf("Greet kind = %q, want method", kind["Greet"])
	}
	if kind["Person"] != "type" {
		t.Errorf("Person kind = %q, want type", kind["Person"])
	}
}
