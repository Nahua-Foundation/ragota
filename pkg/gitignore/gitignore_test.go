package gitignore

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_NoFile(t *testing.T) {
	tmp := t.TempDir()
	m, err := Load(tmp)
	require.NoError(t, err)
	assert.NotNil(t, m)
	assert.Empty(t, m.patterns)
}

func TestLoad_BasicPatterns(t *testing.T) {
	tmp := t.TempDir()
	content := `
# comment
*.log
build/
!important.log
node_modules
`
	err := os.WriteFile(tmp+"/.gitignore", []byte(content), 0o644)
	require.NoError(t, err)

	m, err := Load(tmp)
	require.NoError(t, err)
	assert.Len(t, m.patterns, 4)

	// *.log
	assert.Equal(t, "*.log", m.patterns[0].pattern)
	assert.False(t, m.patterns[0].negate)
	assert.False(t, m.patterns[0].dirOnly)

	// build/
	assert.Equal(t, "build", m.patterns[1].pattern)
	assert.True(t, m.patterns[1].dirOnly)

	// !important.log
	assert.Equal(t, "important.log", m.patterns[2].pattern)
	assert.True(t, m.patterns[2].negate)

	// node_modules
	assert.Equal(t, "node_modules", m.patterns[3].pattern)
	assert.False(t, m.patterns[3].negate)
	assert.False(t, m.patterns[3].dirOnly)
}

func TestMatch_SimpleGlob(t *testing.T) {
	tmp := t.TempDir()
	content := "*.log\n*.tmp\n"
	err := os.WriteFile(tmp+"/.gitignore", []byte(content), 0o644)
	require.NoError(t, err)

	m, err := Load(tmp)
	require.NoError(t, err)

	ignored, negated := m.Match("app.log")
	assert.True(t, ignored)
	assert.False(t, negated)

	ignored, negated = m.Match("data.tmp")
	assert.True(t, ignored)
	assert.False(t, negated)

	ignored, negated = m.Match("main.go")
	assert.False(t, ignored)
	assert.False(t, negated)
}

func TestMatch_DirOnly(t *testing.T) {
	tmp := t.TempDir()
	content := "build/\n"
	err := os.WriteFile(tmp+"/.gitignore", []byte(content), 0o644)
	require.NoError(t, err)

	m, err := Load(tmp)
	require.NoError(t, err)

	ignored, _ := m.Match("build")
	assert.True(t, ignored)

	ignored, _ = m.Match("build/output.o")
	assert.True(t, ignored)
}

func TestMatch_Negation(t *testing.T) {
	tmp := t.TempDir()
	content := "*.log\n!important.log\n"
	err := os.WriteFile(tmp+"/.gitignore", []byte(content), 0o644)
	require.NoError(t, err)

	m, err := Load(tmp)
	require.NoError(t, err)

	ignored, negated := m.Match("app.log")
	assert.True(t, ignored)
	assert.False(t, negated)

	// important.log matches both *.log and !important.log
	// !important.log comes later, so negation wins
	ignored, negated = m.Match("important.log")
	assert.False(t, ignored) // negation overrides
	assert.True(t, negated)
}

func TestMatch_Wildcard(t *testing.T) {
	tmp := t.TempDir()
	content := "node_modules\nvendor\n"
	err := os.WriteFile(tmp+"/.gitignore", []byte(content), 0o644)
	require.NoError(t, err)

	m, err := Load(tmp)
	require.NoError(t, err)

	assert.True(t, m.ShouldSkip("node_modules"))
	assert.True(t, m.ShouldSkip("vendor"))
	assert.False(t, m.ShouldSkip("src"))
}

func TestMatch_DoubleWildcard(t *testing.T) {
	tmp := t.TempDir()
	content := "**/*.log\n"
	err := os.WriteFile(tmp+"/.gitignore", []byte(content), 0o644)
	require.NoError(t, err)

	m, err := Load(tmp)
	require.NoError(t, err)

	ignored, _ := m.Match("app.log")
	assert.True(t, ignored)

	ignored, _ = m.Match("subdir/app.log")
	assert.True(t, ignored)

	ignored, _ = m.Match("a/b/c/app.log")
	assert.True(t, ignored)
}

func TestShouldSkip(t *testing.T) {
	tmp := t.TempDir()
	content := "build/\n*.log\n!important.log\n"
	err := os.WriteFile(tmp+"/.gitignore", []byte(content), 0o644)
	require.NoError(t, err)

	m, err := Load(tmp)
	require.NoError(t, err)

	assert.True(t, m.ShouldSkip("build"))
	assert.True(t, m.ShouldSkip("app.log"))
	assert.False(t, m.ShouldSkip("important.log")) // negation
	assert.False(t, m.ShouldSkip("main.go"))
}

func TestMatch_EmptyMatcher(t *testing.T) {
	m := &Matcher{}
	ignored, negated := m.Match("anything")
	assert.False(t, ignored)
	assert.False(t, negated)
}

func TestMatch_NilMatcher(t *testing.T) {
	var m *Matcher
	ignored, negated := m.Match("anything")
	assert.False(t, ignored)
	assert.False(t, negated)
}

func TestMatch_PrefixedSlash(t *testing.T) {
	tmp := t.TempDir()
	content := "/build\n"
	err := os.WriteFile(tmp+"/.gitignore", []byte(content), 0o644)
	require.NoError(t, err)

	m, err := Load(tmp)
	require.NoError(t, err)

	ignored, _ := m.Match("build")
	assert.True(t, ignored)
}

func TestMatch_SubdirPath(t *testing.T) {
	tmp := t.TempDir()
	content := "*.yaml\n"
	err := os.WriteFile(tmp+"/.gitignore", []byte(content), 0o644)
	require.NoError(t, err)

	m, err := Load(tmp)
	require.NoError(t, err)

	ignored, _ := m.Match("k8s/deployment.yaml")
	assert.True(t, ignored)
}
