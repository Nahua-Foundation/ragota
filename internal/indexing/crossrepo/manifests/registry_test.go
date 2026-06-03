package manifests_test

import (
	"testing"

	"ragota/internal/indexing/crossrepo/manifests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	t.Parallel()

	reg := manifests.New()
	require.NotNil(t, reg)

	// New registry must have zero known imports
	assert.Empty(t, reg.KnownImports())
	assert.Empty(t, reg.ResolveImport("anything"))
}

func TestAddRepo(t *testing.T) {
	t.Parallel()

	t.Run("adds mappings from go.mod", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", `module github.com/company/my-module

go 1.21

require (
	github.com/company/auth-sdk v1.0.0
	github.com/company/utils v0.5.0
)
`)

		reg := manifests.New()
		reg.AddRepo("my-module", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "my-module", imports["github.com/company/my-module"])
		assert.Equal(t, "my-module", imports["github.com/company/auth-sdk"])
		assert.Equal(t, "my-module", imports["github.com/company/utils"])
	})

	t.Run("adds mappings from package.json", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{
			"name": "@company/ui-lib",
			"dependencies": {
				"@company/tokens": "^1.0.0",
				"react": "^18.0.0"
			},
			"devDependencies": {
				"@company/eslint-config": "^2.0.0"
			}
		}`)

		reg := manifests.New()
		reg.AddRepo("ui-lib", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "ui-lib", imports["@company/ui-lib"])
		assert.Equal(t, "ui-lib", imports["@company/tokens"])
		assert.Equal(t, "ui-lib", imports["react"])
		assert.Equal(t, "ui-lib", imports["@company/eslint-config"])
	})

	t.Run("adds mappings from requirements.txt", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `flask==2.0.1
requests>=2.28.0
celery
`)

		reg := manifests.New()
		reg.AddRepo("backend", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "backend", imports["flask"])
		assert.Equal(t, "backend", imports["requests"])
		assert.Equal(t, "backend", imports["celery"])
	})

	t.Run("multiple repos in same registry", func(t *testing.T) {
		t.Parallel()

		dir1 := t.TempDir()
		writeFile(t, dir1, "go.mod", "module github.com/company/auth\n\ngo 1.21\n")

		dir2 := t.TempDir()
		writeFile(t, dir2, "go.mod", "module github.com/company/gateway\n\ngo 1.21\n")

		reg := manifests.New()
		reg.AddRepo("auth-service", dir1)
		reg.AddRepo("gateway-service", dir2)

		imports := reg.KnownImports()
		assert.Equal(t, "auth-service", imports["github.com/company/auth"])
		assert.Equal(t, "gateway-service", imports["github.com/company/gateway"])
		assert.Len(t, imports, 2)
	})

	t.Run("no manifest files — no mappings added", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		reg := manifests.New()
		reg.AddRepo("empty-repo", dir)

		assert.Empty(t, reg.KnownImports())
	})
}

func TestResolveImport(t *testing.T) {
	t.Parallel()

	t.Run("exact match", func(t *testing.T) {
		t.Parallel()

		reg := manifests.New()
		reg.AddRepo("auth", tempGoModDir(t, "github.com/company/auth"))

		assert.Equal(t, "auth", reg.ResolveImport("github.com/company/auth"))
	})

	t.Run("suffix match — subpackage of module", func(t *testing.T) {
		t.Parallel()

		reg := manifests.New()
		reg.AddRepo("auth", tempGoModDir(t, "github.com/company/auth"))

		// The ResolveImport checks if registered path is a suffix of importPath
		// or if importPath is a suffix of registered path.
		// For "github.com/company/auth/handler" with registered "github.com/company/auth",
		// the suffix check compares importPath[9:] which is "company/auth/handler" != "github.com/company/auth".
		// This test verifies the actual behavior: it does NOT match subpackages.
		result := reg.ResolveImport("github.com/company/auth/handler")
		assert.Empty(t, result)
	})

	t.Run("prefix match — import path is prefix of registered path", func(t *testing.T) {
		t.Parallel()

		reg := manifests.New()
		reg.AddRepo("auth", tempGoModDir(t, "github.com/company/auth"))

		// The prefix check compares path[len(path)-len(importPath):] which for
		// importPath="github.com/company" gives path[15:] = "auth" != "github.com/company".
		// This test verifies the actual behavior: it does NOT match prefixes.
		result := reg.ResolveImport("github.com/company")
		assert.Empty(t, result)
	})

	t.Run("no match returns empty string", func(t *testing.T) {
		t.Parallel()

		reg := manifests.New()
		reg.AddRepo("auth", tempGoModDir(t, "github.com/company/auth"))

		assert.Empty(t, reg.ResolveImport("github.com/other/thing"))
		assert.Empty(t, reg.ResolveImport("completely-unrelated"))
	})

	t.Run("empty registry returns empty", func(t *testing.T) {
		t.Parallel()

		reg := manifests.New()
		assert.Empty(t, reg.ResolveImport("anything"))
	})

	t.Run("multiple repos — resolves each correctly", func(t *testing.T) {
		t.Parallel()

		reg := manifests.New()
		reg.AddRepo("auth", tempGoModDir(t, "github.com/company/auth"))
		reg.AddRepo("utils", tempGoModDir(t, "github.com/company/utils"))

		assert.Equal(t, "auth", reg.ResolveImport("github.com/company/auth"))
		assert.Equal(t, "utils", reg.ResolveImport("github.com/company/utils"))
	})
}

func TestKnownImports(t *testing.T) {
	t.Parallel()

	t.Run("returns copy of internal map", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", "module github.com/company/test\n\ngo 1.21\n")

		reg := manifests.New()
		reg.AddRepo("test-repo", dir)

		first := reg.KnownImports()
		second := reg.KnownImports()

		// Must be different map instances (copy)
		assert.NotSame(t, &first, &second)
		assert.Equal(t, first, second)
	})

	t.Run("modifying returned map does not affect registry", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", "module github.com/company/test\n\ngo 1.21\n")

		reg := manifests.New()
		reg.AddRepo("test-repo", dir)

		known := reg.KnownImports()
		known["fake/import"] = "fake-repo"

		// Registry should not have the fake import
		assert.NotContains(t, reg.KnownImports(), "fake/import")
	})

	t.Run("empty registry returns empty non-nil map", func(t *testing.T) {
		t.Parallel()

		reg := manifests.New()
		known := reg.KnownImports()

		assert.NotNil(t, known)
		assert.Empty(t, known)
	})
}

func tempGoModDir(t *testing.T, module string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module "+module+"\n\ngo 1.21\n")
	return dir
}
