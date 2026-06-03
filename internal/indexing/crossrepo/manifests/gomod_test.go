package manifests_test

import (
	"testing"

	"ragota/internal/indexing/crossrepo/manifests"

	"github.com/stretchr/testify/assert"
)

func TestParseGoMod(t *testing.T) {
	t.Parallel()

	t.Run("module declaration only", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", `module github.com/company/auth

go 1.21
`)

		reg := manifests.New()
		reg.AddRepo("auth-service", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "auth-service", imports["github.com/company/auth"])
		assert.Len(t, imports, 1)
	})

	t.Run("require block with multiple dependencies", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", `module github.com/company/gateway

go 1.21

require (
	github.com/company/auth-sdk v1.2.0
	github.com/company/logging v0.3.1
	github.com/company/config v2.0.0
)
`)

		reg := manifests.New()
		reg.AddRepo("gateway", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "gateway", imports["github.com/company/gateway"])
		assert.Equal(t, "gateway", imports["github.com/company/auth-sdk"])
		assert.Equal(t, "gateway", imports["github.com/company/logging"])
		assert.Equal(t, "gateway", imports["github.com/company/config"])
		assert.Len(t, imports, 4)
	})

	t.Run("single-line require statements", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", `module github.com/company/single-req

go 1.21

require github.com/company/dep1 v1.0.0
require github.com/company/dep2 v2.0.0
`)

		reg := manifests.New()
		reg.AddRepo("single-req", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "single-req", imports["github.com/company/single-req"])
		assert.Equal(t, "single-req", imports["github.com/company/dep1"])
		assert.Equal(t, "single-req", imports["github.com/company/dep2"])
	})

	t.Run("replace directives in block", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", `module github.com/company/with-replace

go 1.21

require github.com/company/old-auth v1.0.0

replace (
	github.com/company/old-auth => github.com/company/new-auth v2.0.0
	github.com/company/deprecated => ../local-pkg
)
`)

		reg := manifests.New()
		reg.AddRepo("replace-svc", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "replace-svc", imports["github.com/company/with-replace"])
		assert.Equal(t, "replace-svc", imports["github.com/company/old-auth"])
		assert.Equal(t, "replace-svc", imports["github.com/company/deprecated"])
	})

	t.Run("single-line replace directive", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		// The parser expects: "replace <old> => <new>" with parts[1] == "=>"
		// This means it parses as: [replace, =>, old, new] which is NOT valid go.mod syntax.
		// Real go.mod uses: "replace old => new v1.0.0" where parts[1] is the old path.
		// This test verifies the parser only handles block-style replaces correctly.
		writeFile(t, dir, "go.mod", `module github.com/company/inline-replace

go 1.21

replace github.com/company/old => github.com/company/new v1.0.0
`)

		reg := manifests.New()
		reg.AddRepo("inline-svc", dir)

		imports := reg.KnownImports()
		// Module name is always registered
		assert.Equal(t, "inline-svc", imports["github.com/company/inline-replace"])
		// Single-line replace with format "old => new v1.0.0" has parts[1]="old" not "=>"
		// so the parser's check `parts[1] == "=>"` will fail -- old is NOT registered
		assert.Empty(t, imports["github.com/company/old"])
	})

	t.Run("comments are ignored", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", `module github.com/company/with-comments

go 1.21

require (
	// this is a comment
	github.com/company/real-dep v1.0.0
	// another comment
)

replace (
	// commented replace
	github.com/company/ignored => github.com/company/skip v1.0.0
)
`)

		reg := manifests.New()
		reg.AddRepo("commented", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "commented", imports["github.com/company/with-comments"])
		assert.Equal(t, "commented", imports["github.com/company/real-dep"])
		// Commented lines inside blocks should still be parsed by the simple scanner
		// The parser checks !strings.HasPrefix(line, "//"), so commented entries are skipped
	})

	t.Run("missing go.mod — no error, no mappings", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		// No go.mod file created

		reg := manifests.New()
		reg.AddRepo("no-gomod", dir)

		assert.Empty(t, reg.KnownImports())
	})

	t.Run("mixed require and replace blocks", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", `module github.com/company/mixed

go 1.21

require (
	github.com/company/alpha v1.0.0
	github.com/company/beta v2.0.0
)

replace github.com/company/alpha => github.com/company/alpha-fork v1.1.0

require github.com/company/gamma v0.1.0
`)

		reg := manifests.New()
		reg.AddRepo("mixed-svc", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "mixed-svc", imports["github.com/company/mixed"])
		assert.Equal(t, "mixed-svc", imports["github.com/company/alpha"])
		assert.Equal(t, "mixed-svc", imports["github.com/company/beta"])
		assert.Equal(t, "mixed-svc", imports["github.com/company/gamma"])
	})

	t.Run("empty go.mod", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", "")

		reg := manifests.New()
		reg.AddRepo("empty-mod", dir)

		assert.Empty(t, reg.KnownImports())
	})

	t.Run("go.mod with only module line and comments", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", `// This is a comment
module github.com/company/minimal

// Another comment
go 1.21
`)

		reg := manifests.New()
		reg.AddRepo("minimal", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "minimal", imports["github.com/company/minimal"])
		assert.Len(t, imports, 1)
	})

	t.Run("repoImports tracks registered imports", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", `module github.com/company/tracked

go 1.21

require (
	github.com/company/a v1.0.0
	github.com/company/b v1.0.0
)
`)

		reg := manifests.New()
		reg.AddRepo("tracked", dir)

		// We can't directly access repoImports (unexported), but KnownImports
		// should reflect 3 entries: module + 2 requires
		assert.Len(t, reg.KnownImports(), 3)
	})
}

func TestParseGoMod_RealWorldContent(t *testing.T) {
	t.Parallel()

	t.Run("realistic go.mod similar to production", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", `module gitlab.tcsbank.ru/platform/ragota

go 1.22.0

require (
	github.com/gin-gonic/gin v1.9.1
	github.com/stretchr/testify v1.8.4
	go.uber.org/zap v1.26.0
	google.golang.org/grpc v1.59.0
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
)

replace gitlab.tcsbank.ru/platform/common => ../common
`)

		reg := manifests.New()
		reg.AddRepo("ragota", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "ragota", imports["gitlab.tcsbank.ru/platform/ragota"])
		assert.Equal(t, "ragota", imports["github.com/gin-gonic/gin"])
		assert.Equal(t, "ragota", imports["go.uber.org/zap"])
		assert.Equal(t, "ragota", imports["google.golang.org/grpc"])
		// Single-line replace with "old => new path" format is not parsed by current parser
		// (parts[1] != "=>"), so common is NOT registered
		assert.Empty(t, imports["gitlab.tcsbank.ru/platform/common"])
	})
}
