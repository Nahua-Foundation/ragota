package manifests_test

import (
	"testing"

	"ragota/internal/indexing/crossrepo/manifests"

	"github.com/stretchr/testify/assert"
)

func TestParsePackageJSON(t *testing.T) {
	t.Parallel()

	t.Run("name and dependencies", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{
			"name": "my-app",
			"version": "1.0.0",
			"dependencies": {
				"express": "^4.18.0",
				"lodash": "^4.17.21"
			}
		}`)

		reg := manifests.New()
		reg.AddRepo("my-app", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "my-app", imports["my-app"])
		assert.Equal(t, "my-app", imports["express"])
		assert.Equal(t, "my-app", imports["lodash"])
		assert.Len(t, imports, 3)
	})

	t.Run("devDependencies are also registered", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{
			"name": "test-pkg",
			"dependencies": {
				"react": "^18.0.0"
			},
			"devDependencies": {
				"jest": "^29.0.0",
				"typescript": "^5.0.0"
			}
		}`)

		reg := manifests.New()
		reg.AddRepo("test-pkg", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "test-pkg", imports["test-pkg"])
		assert.Equal(t, "test-pkg", imports["react"])
		assert.Equal(t, "test-pkg", imports["jest"])
		assert.Equal(t, "test-pkg", imports["typescript"])
		assert.Len(t, imports, 4)
	})

	t.Run("scoped packages with @company prefix", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{
			"name": "@company/auth-sdk",
			"dependencies": {
				"@company/tokens": "^1.0.0",
				"@company/eslint-config": "^2.0.0"
			},
			"devDependencies": {
				"@company/jest-presets": "^1.0.0"
			}
		}`)

		reg := manifests.New()
		reg.AddRepo("auth-sdk", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "auth-sdk", imports["@company/auth-sdk"])
		assert.Equal(t, "auth-sdk", imports["@company/tokens"])
		assert.Equal(t, "auth-sdk", imports["@company/eslint-config"])
		assert.Equal(t, "auth-sdk", imports["@company/jest-presets"])
		assert.Len(t, imports, 4)
	})

	t.Run("missing package.json — no error, no mappings", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		// No package.json created

		reg := manifests.New()
		reg.AddRepo("no-pkgjson", dir)

		assert.Empty(t, reg.KnownImports())
	})

	t.Run("invalid JSON — no error, no mappings", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{
			"name": "broken",
			"dependencies": {
				"express": "^4.0.0",
			}
		}`)

		reg := manifests.New()
		reg.AddRepo("broken-pkg", dir)

		assert.Empty(t, reg.KnownImports())
	})

	t.Run("empty object — only name if present", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{}`)

		reg := manifests.New()
		reg.AddRepo("empty-pkg", dir)

		assert.Empty(t, reg.KnownImports())
	})

	t.Run("package with no name but has deps", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{
			"dependencies": {
				"axios": "^1.6.0",
				"moment": "^2.30.0"
			}
		}`)

		reg := manifests.New()
		reg.AddRepo("noname-pkg", dir)

		imports := reg.KnownImports()
		// No name registered, but deps are
		assert.Empty(t, imports[""])
		assert.Equal(t, "noname-pkg", imports["axios"])
		assert.Equal(t, "noname-pkg", imports["moment"])
		assert.Len(t, imports, 2)
	})

	t.Run("dependency name collision between deps and devDeps", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{
			"name": "collision-pkg",
			"dependencies": {
				"shared-dep": "^1.0.0"
			},
			"devDependencies": {
				"shared-dep": "^2.0.0"
			}
		}`)

		reg := manifests.New()
		reg.AddRepo("collision-pkg", dir)

		imports := reg.KnownImports()
		// Map keys are unique — should still resolve to same repo
		assert.Equal(t, "collision-pkg", imports["shared-dep"])
		assert.Equal(t, "collision-pkg", imports["collision-pkg"])
		assert.Len(t, imports, 2)
	})

	t.Run("mixed scoped and unscoped packages", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{
			"name": "@company/mixed-lib",
			"dependencies": {
				"@company/internal": "^1.0.0",
				"react": "^18.0.0",
				"@types/node": "^20.0.0"
			}
		}`)

		reg := manifests.New()
		reg.AddRepo("mixed-lib", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "mixed-lib", imports["@company/mixed-lib"])
		assert.Equal(t, "mixed-lib", imports["@company/internal"])
		assert.Equal(t, "mixed-lib", imports["react"])
		assert.Equal(t, "mixed-lib", imports["@types/node"])
		assert.Len(t, imports, 4)
	})
}

func TestParsePackageJSON_RealWorldContent(t *testing.T) {
	t.Parallel()

	t.Run("realistic package.json similar to production frontend", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{
			"name": "@company/portal-frontend",
			"version": "3.2.1",
			"private": true,
			"dependencies": {
				"@company/ui-kit": "^4.0.0",
				"@company/auth-client": "^2.1.0",
				"react": "^18.2.0",
				"react-dom": "^18.2.0",
				"react-router-dom": "^6.20.0",
				"axios": "^1.6.0"
			},
			"devDependencies": {
				"@company/eslint-config-frontend": "^3.0.0",
				"@testing-library/react": "^14.0.0",
				"typescript": "^5.3.0",
				"vite": "^5.0.0"
			}
		}`)

		reg := manifests.New()
		reg.AddRepo("portal-frontend", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "portal-frontend", imports["@company/portal-frontend"])
		assert.Equal(t, "portal-frontend", imports["@company/ui-kit"])
		assert.Equal(t, "portal-frontend", imports["@company/auth-client"])
		assert.Equal(t, "portal-frontend", imports["react"])
		assert.Equal(t, "portal-frontend", imports["@company/eslint-config-frontend"])
		assert.Equal(t, "portal-frontend", imports["vite"])
		// 1 name + 6 deps + 4 devDeps = 11
		assert.Len(t, imports, 11)
	})
}
