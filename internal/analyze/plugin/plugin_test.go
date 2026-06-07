package plugin_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ragota/internal/analyze/plugin/builtin"
)

func TestGoPlugin_IsTestFile(t *testing.T) {
	p := &builtin.GoPlugin{}

	assert.True(t, p.IsTestFile("user_test.go"))
	assert.True(t, p.IsTestFile("handler_test.go"))
	assert.False(t, p.IsTestFile("user.go"))
	assert.False(t, p.IsTestFile("handler.go"))
}

func TestGoPlugin_ExtractImports(t *testing.T) {
	p := &builtin.GoPlugin{}

	lines := []string{
		`package main`,
		``,
		`import (`,
		`	"fmt"`,
		`	"strings"`,
		`	"ragota/internal/analyze"`,
		`)`,
		``,
		`func main() {`,
	}

	imports := p.ExtractImports(lines)
	assert.Equal(t, 3, len(imports))
	assert.Contains(t, imports, "fmt")
	assert.Contains(t, imports, "strings")
	assert.Contains(t, imports, "ragota/internal/analyze")
}

func TestGoPlugin_ExtractImports_SingleLine(t *testing.T) {
	p := &builtin.GoPlugin{}

	lines := []string{
		`package main`,
		``,
		`import "fmt"`,
		`import "strings"`,
		``,
		`func main() {`,
	}

	imports := p.ExtractImports(lines)
	assert.Equal(t, 2, len(imports))
}

func TestGoPlugin_ExtractSignatures(t *testing.T) {
	p := &builtin.GoPlugin{}

	lines := []string{
		`package main`,
		``,
		`func main() {`,
		`	fmt.Println("hello")`,
		`}`,
		``,
		`type User struct {`,
		`	ID   int`,
		`	Name string`,
		`}`,
		``,
		`func (u *User) GetName() string {`,
	}

	sigs := p.ExtractSignatures(lines)
	assert.GreaterOrEqual(t, len(sigs), 2)

	// Should contain func and type
	hasFunc := false
	hasType := false
	for _, sig := range sigs {
		if strings.Contains(sig, "func main") {
			hasFunc = true
		}
		if strings.Contains(sig, "type User") {
			hasType = true
		}
	}
	assert.True(t, hasFunc, "should extract func signature")
	assert.True(t, hasType, "should extract type signature")
}

func TestTypeScriptPlugin_IsTestFile(t *testing.T) {
	p := &builtin.TypeScriptPlugin{}

	assert.True(t, p.IsTestFile("user.test.ts"))
	assert.True(t, p.IsTestFile("handler.spec.tsx"))
	assert.True(t, p.IsTestFile("api.test.js"))
	assert.False(t, p.IsTestFile("user.ts"))
	assert.False(t, p.IsTestFile("handler.tsx"))
}

func TestTypeScriptPlugin_ExtractImports(t *testing.T) {
	p := &builtin.TypeScriptPlugin{}

	lines := []string{
		`import { User } from "./types";`,
		`import * as utils from "./utils";`,
		`const fs = require("fs");`,
		``,
		`export function main() {`,
	}

	imports := p.ExtractImports(lines)
	assert.GreaterOrEqual(t, len(imports), 2)
}

func TestPythonPlugin_IsTestFile(t *testing.T) {
	p := &builtin.PythonPlugin{}

	assert.True(t, p.IsTestFile("test_user.py"))
	assert.True(t, p.IsTestFile("user_test.py"))
	assert.False(t, p.IsTestFile("user.py"))
	assert.False(t, p.IsTestFile("api.py"))
}

func TestPythonPlugin_ExtractImports(t *testing.T) {
	p := &builtin.PythonPlugin{}

	lines := []string{
		`import os`,
		`import sys`,
		`from typing import List`,
		`from ragota.analyze import Plugin`,
		``,
		`def main():`,
	}

	imports := p.ExtractImports(lines)
	assert.GreaterOrEqual(t, len(imports), 3)
}

func TestJavaPlugin_IsTestFile(t *testing.T) {
	p := &builtin.JavaPlugin{}

	assert.True(t, p.IsTestFile("TestUser.java"))
	assert.True(t, p.IsTestFile("UserTest.java"))
	assert.True(t, p.IsTestFile("src/test/java/UserTest.java"))
	assert.False(t, p.IsTestFile("User.java"))
}

func TestDefaultRegistry(t *testing.T) {
	reg := builtin.DefaultRegistry()

	// Should have built-in plugins
	goPlugin, ok := reg.Get("go")
	require.True(t, ok, "should have Go plugin")
	assert.Equal(t, "go", goPlugin.Name())
	assert.Contains(t, goPlugin.Extensions(), ".go")

	tsPlugin, ok := reg.Get("typescript")
	require.True(t, ok, "should have TypeScript plugin")
	assert.Contains(t, tsPlugin.Extensions(), ".ts")
	assert.Contains(t, tsPlugin.Extensions(), ".tsx")

	pyPlugin, ok := reg.Get("python")
	require.True(t, ok, "should have Python plugin")
	assert.Contains(t, pyPlugin.Extensions(), ".py")

	javaPlugin, ok := reg.Get("java")
	require.True(t, ok, "should have Java plugin")
	assert.Contains(t, javaPlugin.Extensions(), ".java")

	// Should have generic plugins
	rustPlugin, ok := reg.Get("rust")
	require.True(t, ok, "should have Rust plugin")
	assert.Contains(t, rustPlugin.Extensions(), ".rs")
}

func TestRegistry_GetByExtension(t *testing.T) {
	reg := builtin.DefaultRegistry()

	p := reg.GetByExtension(".go")
	require.NotNil(t, p)
	assert.Equal(t, "go", p.Name())

	p = reg.GetByExtension(".ts")
	require.NotNil(t, p)
	assert.Equal(t, "typescript", p.Name())

	p = reg.GetByExtension(".py")
	require.NotNil(t, p)
	assert.Equal(t, "python", p.Name())

	p = reg.GetByExtension(".unknown")
	assert.Nil(t, p)
}
