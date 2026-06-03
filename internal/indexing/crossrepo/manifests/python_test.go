package manifests_test

import (
	"testing"

	"ragota/internal/indexing/crossrepo/manifests"

	"github.com/stretchr/testify/assert"
)

func TestParseRequirementsTXT(t *testing.T) {
	t.Parallel()

	t.Run("simple packages without versions", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `flask
requests
celery
`)

		reg := manifests.New()
		reg.AddRepo("py-backend", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "py-backend", imports["flask"])
		assert.Equal(t, "py-backend", imports["requests"])
		assert.Equal(t, "py-backend", imports["celery"])
		assert.Len(t, imports, 3)
	})

	t.Run("version specifiers with ==", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `flask==2.0.1
requests==2.28.0
celery==5.3.1
`)

		reg := manifests.New()
		reg.AddRepo("pinned-py", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "pinned-py", imports["flask"])
		assert.Equal(t, "pinned-py", imports["requests"])
		assert.Equal(t, "pinned-py", imports["celery"])
	})

	t.Run("version specifiers with >=", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `flask>=2.0.0
requests>=2.28.0
celery>=5.0
`)

		reg := manifests.New()
		reg.AddRepo("minver-py", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "minver-py", imports["flask"])
		assert.Equal(t, "minver-py", imports["requests"])
		assert.Equal(t, "minver-py", imports["celery"])
	})

	t.Run("version specifiers with ~=", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `flask~=2.0
requests~=2.28.0
`)

		reg := manifests.New()
		reg.AddRepo("compatible-py", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "compatible-py", imports["flask"])
		assert.Equal(t, "compatible-py", imports["requests"])
	})

	t.Run("mixed version specifiers", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `flask==2.0.1
requests>=2.28,<3.0
celery~=5.3
sqlalchemy>1.4
pytest!=7.0.0
numpy<=1.26
`)

		reg := manifests.New()
		reg.AddRepo("mixed-ver-py", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "mixed-ver-py", imports["flask"])
		assert.Equal(t, "mixed-ver-py", imports["requests"])
		assert.Equal(t, "mixed-ver-py", imports["celery"])
		assert.Equal(t, "mixed-ver-py", imports["sqlalchemy"])
		assert.Equal(t, "mixed-ver-py", imports["pytest"])
		assert.Equal(t, "mixed-ver-py", imports["numpy"])
		assert.Len(t, imports, 6)
	})

	t.Run("comments are ignored", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `# This is a comment
flask==2.0.1
# Another comment about requests
requests>=2.28.0
`)

		reg := manifests.New()
		reg.AddRepo("commented-py", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "commented-py", imports["flask"])
		assert.Equal(t, "commented-py", imports["requests"])
		assert.Len(t, imports, 2)
	})

	t.Run("blank lines are ignored", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `flask==2.0.1

requests>=2.28.0

celery
`)

		reg := manifests.New()
		reg.AddRepo("blanks-py", dir)

		imports := reg.KnownImports()
		assert.Len(t, imports, 3)
		assert.Equal(t, "blanks-py", imports["flask"])
		assert.Equal(t, "blanks-py", imports["requests"])
		assert.Equal(t, "blanks-py", imports["celery"])
	})

	t.Run("-r flag for recursive includes is skipped", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `-r base.txt
flask==2.0.1
-r dev.txt
`)

		reg := manifests.New()
		reg.AddRepo("recursive-py", dir)

		imports := reg.KnownImports()
		// -r lines should be skipped
		assert.Equal(t, "recursive-py", imports["flask"])
		assert.Len(t, imports, 1)
	})

	t.Run("-e flag for editable installs is skipped", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `-e ./local-pkg
flask==2.0.1
-e git+https://github.com/user/repo.git#egg=mylib
`)

		reg := manifests.New()
		reg.AddRepo("editable-py", dir)

		imports := reg.KnownImports()
		// -e lines should be skipped
		assert.Equal(t, "editable-py", imports["flask"])
		assert.Len(t, imports, 1)
	})

	t.Run("dash-to-underscore normalization", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `my-package==1.0.0
some-other-lib>=2.0
`)

		reg := manifests.New()
		reg.AddRepo("normalized-py", dir)

		imports := reg.KnownImports()
		// Both dash and underscore versions should be registered
		assert.Equal(t, "normalized-py", imports["my-package"])
		assert.Equal(t, "normalized-py", imports["my_package"])
		assert.Equal(t, "normalized-py", imports["some-other-lib"])
		assert.Equal(t, "normalized-py", imports["some_other_lib"])
	})

	t.Run("missing requirements.txt — no error, no mappings", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		// No requirements.txt created

		reg := manifests.New()
		reg.AddRepo("no-reqs", dir)

		assert.Empty(t, reg.KnownImports())
	})

	t.Run("empty requirements.txt", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", "")

		reg := manifests.New()
		reg.AddRepo("empty-reqs", dir)

		assert.Empty(t, reg.KnownImports())
	})

	t.Run("only comments and blank lines", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `# No actual dependencies here
# Just comments

`)

		reg := manifests.New()
		reg.AddRepo("comments-only-py", dir)

		assert.Empty(t, reg.KnownImports())
	})

	t.Run("inline comments after package", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `flask==2.0.1  # web framework
requests>=2.28  # HTTP library
`)

		reg := manifests.New()
		reg.AddRepo("inline-comment-py", dir)

		imports := reg.KnownImports()
		// The parser extracts "flask" and "requests" before the version specifier
		// Inline comments after version are part of the line but package name is still extracted
		assert.Equal(t, "inline-comment-py", imports["flask"])
		assert.Equal(t, "inline-comment-py", imports["requests"])
	})

	t.Run("complex realistic requirements.txt", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeFile(t, dir, "requirements.txt", `# Core dependencies
flask==2.3.2
sqlalchemy>=2.0.0,<3.0.0
celery~=5.3.0

# Auth
pyjwt==2.8.0
cryptography>=41.0

# Monitoring
prometheus-client>=0.19
structlog>=23.1.0

# Dev dependencies -r dev-requirements.txt
`)

		reg := manifests.New()
		reg.AddRepo("realistic-py", dir)

		imports := reg.KnownImports()
		assert.Equal(t, "realistic-py", imports["flask"])
		assert.Equal(t, "realistic-py", imports["sqlalchemy"])
		assert.Equal(t, "realistic-py", imports["celery"])
		assert.Equal(t, "realistic-py", imports["pyjwt"])
		assert.Equal(t, "realistic-py", imports["cryptography"])
		assert.Equal(t, "realistic-py", imports["prometheus_client"])
		assert.Equal(t, "realistic-py", imports["prometheus-client"])
		assert.Equal(t, "realistic-py", imports["structlog"])
		// -r line should not be registered
		assert.NotContains(t, imports, "dev-requirements.txt")
	})
}
