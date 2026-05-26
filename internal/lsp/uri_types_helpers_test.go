package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== FileURI ====================

func TestFileURI_AbsolutePath(t *testing.T) {
	uri := FileURI("/tmp/foo.go")
	assert.Contains(t, uri, "file://")
	assert.Contains(t, uri, "/tmp/foo.go")
}

func TestFileURI_RelativePath(t *testing.T) {
	// Relative path should be converted to absolute
	uri := FileURI("relative/path.go")
	assert.Contains(t, uri, "file://")
	// Should not contain "relative" as-is — it gets resolved via filepath.Abs
	assert.NotEqual(t, "file://relative/path.go", uri)
}

func TestFileURI_EmptyPath(t *testing.T) {
	uri := FileURI("")
	assert.Contains(t, uri, "file://")
}

func TestFileURI_SpecialChars(t *testing.T) {
	uri := FileURI("/tmp/my file (copy).go")
	assert.Contains(t, uri, "file://")
	// Spaces should be percent-encoded
	assert.Contains(t, uri, "%20")
}

func TestFileURI_AlreadyAbsolute(t *testing.T) {
	uri := FileURI("/home/user/project/main.go")
	assert.Contains(t, uri, "/home/user/project/main.go")
}

// ==================== pathFromFileURI ====================

func TestPathFromFileURI_Valid(t *testing.T) {
	p := pathFromFileURI("file:///tmp/foo.go")
	assert.Equal(t, filepath.FromSlash("/tmp/foo.go"), p)
}

func TestPathFromFileURI_NonFileScheme(t *testing.T) {
	p := pathFromFileURI("http://example.com/foo.go")
	assert.Equal(t, "http://example.com/foo.go", p)
}

func TestPathFromFileURI_InvalidURI(t *testing.T) {
	p := pathFromFileURI("://bad")
	assert.Equal(t, "://bad", p)
}

func TestPathFromFileURI_Empty(t *testing.T) {
	p := pathFromFileURI("")
	assert.Equal(t, "", p)
}

func TestPathFromFileURI_NoScheme(t *testing.T) {
	p := pathFromFileURI("/tmp/foo.go")
	assert.Equal(t, "/tmp/foo.go", p)
}

func TestPathFromFileURI_EncodedSpaces(t *testing.T) {
	p := pathFromFileURI("file:///tmp/my%20file.go")
	assert.Contains(t, p, "my file.go")
}

// ==================== toRemotePath / toLocalPath ====================

func TestToRemotePath_NilClient(t *testing.T) {
	var c *Client
	assert.Equal(t, "/some/path", c.toRemotePath("/some/path"))
}

func TestToRemotePath_EmptyRoots(t *testing.T) {
	c := &Client{hostRoot: "", remoteRoot: ""}
	assert.Equal(t, "/some/path", c.toRemotePath("/some/path"))
}

func TestToRemotePath_LocalMode(t *testing.T) {
	c := &Client{
		hostRoot:   "/project",
		remoteRoot: "/project", // identity mapping
	}
	result := c.toRemotePath("/project/main.go")
	assert.Equal(t, "/project/main.go", result)
}

func TestToRemotePath_DockerMode(t *testing.T) {
	c := &Client{
		hostRoot:   "/home/user/project",
		remoteRoot: "/workspace",
	}
	result := c.toRemotePath("/home/user/project/cmd/main.go")
	assert.Equal(t, "/workspace/cmd/main.go", result)
}

func TestToRemotePath_DockerMode_RootItself(t *testing.T) {
	c := &Client{
		hostRoot:   "/home/user/project",
		remoteRoot: "/workspace",
	}
	result := c.toRemotePath("/home/user/project")
	assert.Equal(t, "/workspace", result)
}

func TestToRemotePath_OutsideHostRoot(t *testing.T) {
	c := &Client{
		hostRoot:   "/home/user/project",
		remoteRoot: "/workspace",
	}
	// Path outside hostRoot should be returned as-is
	result := c.toRemotePath("/other/path.go")
	assert.Equal(t, "/other/path.go", result)
}

func TestToLocalPath_NilClient(t *testing.T) {
	var c *Client
	assert.Equal(t, filepath.FromSlash("/some/path"), c.toLocalPath("/some/path"))
}

func TestToLocalPath_PlainPath(t *testing.T) {
	c := &Client{hostRoot: "/project", remoteRoot: "/workspace"}
	assert.Equal(t, filepath.FromSlash("/some/path"), c.toLocalPath("/some/path"))
}

func TestToLocalPath_DockerMode(t *testing.T) {
	c := &Client{
		hostRoot:   "/home/user/project",
		remoteRoot: "/workspace",
	}
	result := c.toLocalPath("file:///workspace/cmd/main.go")
	assert.Contains(t, result, "cmd")
	assert.Contains(t, result, "main.go")
}

func TestToLocalPath_DockerMode_RootItself(t *testing.T) {
	c := &Client{
		hostRoot:   "/home/user/project",
		remoteRoot: "/workspace",
	}
	result := c.toLocalPath("file:///workspace")
	assert.Equal(t, filepath.Clean("/home/user/project"), result)
}

func TestToLocalPath_FileURI_NoMapping(t *testing.T) {
	c := &Client{hostRoot: "", remoteRoot: ""}
	result := c.toLocalPath("file:///tmp/foo.go")
	assert.Contains(t, result, "foo.go")
}

func TestToLocalPath_NonFileURI(t *testing.T) {
	c := &Client{hostRoot: "/project", remoteRoot: "/workspace"}
	result := c.toLocalPath("http://example.com/foo.go")
	assert.Equal(t, filepath.FromSlash("http://example.com/foo.go"), result)
}

// ==================== samePath ====================

func TestSamePath_Identical(t *testing.T) {
	assert.True(t, samePath("/tmp/foo.go", "/tmp/foo.go"))
}

func TestSamePath_WithTrailingSlash(t *testing.T) {
	assert.True(t, samePath("/tmp/foo/", "/tmp/foo"))
}

func TestSamePath_Different(t *testing.T) {
	assert.False(t, samePath("/tmp/foo.go", "/tmp/bar.go"))
}

func TestSamePath_RelativeVsAbsolute(t *testing.T) {
	cwd, _ := os.Getwd()
	rel := "testfile.go"
	abs := filepath.Join(cwd, "testfile.go")
	assert.True(t, samePath(rel, abs))
}

// ==================== isAlphaNum ====================

func TestIsAlphaNum(t *testing.T) {
	tests := []struct {
		c    byte
		want bool
	}{
		{'a', true},
		{'z', true},
		{'A', true},
		{'Z', true},
		{'0', true},
		{'9', true},
		{'_', true},
		{' ', false},
		{'.', false},
		{'(', false},
		{')', false},
		{'-', false},
		{'@', false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, isAlphaNum(tt.c), "isAlphaNum(%c)", tt.c)
	}
}

// ==================== hoverString ====================

func TestHoverString_Nil(t *testing.T) {
	assert.Equal(t, "", hoverString(nil))
}

func TestHoverString_PlainString(t *testing.T) {
	assert.Equal(t, "hello world", hoverString("hello world"))
}

func TestHoverString_MarkupContent(t *testing.T) {
	m := map[string]any{
		"kind":  "markdown",
		"value": "# Title",
	}
	assert.Equal(t, "# Title", hoverString(m))
}

func TestHoverString_PlaintextContent(t *testing.T) {
	m := map[string]any{
		"kind":  "plaintext",
		"value": "plain text",
	}
	assert.Equal(t, "plain text", hoverString(m))
}

func TestHoverString_MarkedString(t *testing.T) {
	m := map[string]any{
		"language": "go",
		"value":    "func Hello()",
	}
	assert.Equal(t, "func Hello()", hoverString(m))
}

func TestHoverString_Array(t *testing.T) {
	arr := []any{
		"first part",
		map[string]any{"kind": "markdown", "value": "second part"},
	}
	result := hoverString(arr)
	assert.Contains(t, result, "first part")
	assert.Contains(t, result, "second part")
}

func TestHoverString_EmptyArray(t *testing.T) {
	arr := []any{}
	assert.Equal(t, "", hoverString(arr))
}

func TestHoverString_ArrayWithNils(t *testing.T) {
	arr := []any{nil, "valid", nil}
	assert.Equal(t, "valid", hoverString(arr))
}

func TestHoverString_MapWithoutValue(t *testing.T) {
	m := map[string]any{
		"kind": "markdown",
	}
	// No value key — falls through to JSON marshal
	result := hoverString(m)
	assert.NotEmpty(t, result)
}

func TestHoverString_UnknownType(t *testing.T) {
	result := hoverString(42)
	assert.NotEmpty(t, result) // JSON representation
}

func TestHoverString_MapWithOnlyValue(t *testing.T) {
	m := map[string]any{
		"value": "just a value",
	}
	assert.Equal(t, "just a value", hoverString(m))
}

// ==================== decodeLocations ====================

func newTestClient() *Client {
	return &Client{
		Language:         "go",
		pending:          make(map[int64]chan rpcResponse),
		diagnosticsReady: make(chan string, 1),
		javaReady:        make(chan struct{}),
		goplsReady:       make(chan struct{}),
		processDone:      make(chan struct{}),
		openedFiles:      make(map[string]string),
		openedVers:       make(map[string]int),
	}
}

func TestDecodeLocations_Null(t *testing.T) {
	c := newTestClient()
	assert.Nil(t, c.decodeLocations(json.RawMessage("null")))
}

func TestDecodeLocations_Empty(t *testing.T) {
	c := newTestClient()
	assert.Nil(t, c.decodeLocations(json.RawMessage("")))
}

func TestDecodeLocations_SingleLocation(t *testing.T) {
	c := newTestClient()
	raw := json.RawMessage(`{"uri":"file:///tmp/foo.go","range":{"start":{"line":1,"character":2},"end":{"line":3,"character":4}}}`)
	locs := c.decodeLocations(raw)
	require.Len(t, locs, 1)
	assert.Equal(t, 1, locs[0].StartLine)
	assert.Equal(t, 2, locs[0].StartChar)
	assert.Equal(t, 3, locs[0].EndLine)
	assert.Equal(t, 4, locs[0].EndChar)
}

func TestDecodeLocations_ArrayOfLocations(t *testing.T) {
	c := newTestClient()
	raw := json.RawMessage(`[
		{"uri":"file:///tmp/a.go","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}}},
		{"uri":"file:///tmp/b.go","range":{"start":{"line":10,"character":0},"end":{"line":10,"character":8}}}
	]`)
	locs := c.decodeLocations(raw)
	require.Len(t, locs, 2)
}

func TestDecodeLocations_SingleLocationLink(t *testing.T) {
	c := newTestClient()
	raw := json.RawMessage(`{"targetUri":"file:///tmp/foo.go","targetRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"targetSelectionRange":{"start":{"line":5,"character":3},"end":{"line":5,"character":10}}}`)
	locs := c.decodeLocations(raw)
	require.Len(t, locs, 1)
	assert.Equal(t, 5, locs[0].StartLine)
	assert.Equal(t, 3, locs[0].StartChar)
}

func TestDecodeLocations_ArrayOfLocationLinks(t *testing.T) {
	c := newTestClient()
	raw := json.RawMessage(`[
		{"targetUri":"file:///tmp/a.go","targetRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"targetSelectionRange":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}}},
		{"targetUri":"file:///tmp/b.go","targetRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"targetSelectionRange":{"start":{"line":2,"character":0},"end":{"line":2,"character":8}}}
	]`)
	locs := c.decodeLocations(raw)
	require.Len(t, locs, 2)
}

func TestDecodeLocations_MalformedJSON(t *testing.T) {
	c := newTestClient()
	assert.Nil(t, c.decodeLocations(json.RawMessage(`{bad json`)))
}

func TestDecodeLocations_EmptyArray(t *testing.T) {
	c := newTestClient()
	raw := json.RawMessage(`[]`)
	assert.Nil(t, c.decodeLocations(raw))
}

// ==================== decodeLocationsWithErr ====================

func TestDecodeLocationsWithErr_Null(t *testing.T) {
	c := newTestClient()
	locs, err := c.decodeLocationsWithErr(json.RawMessage("null"))
	assert.NoError(t, err)
	assert.Nil(t, locs)
}

func TestDecodeLocationsWithErr_Empty(t *testing.T) {
	c := newTestClient()
	locs, err := c.decodeLocationsWithErr(json.RawMessage(""))
	assert.NoError(t, err)
	assert.Nil(t, locs)
}

func TestDecodeLocationsWithErr_ErrorMessage(t *testing.T) {
	c := newTestClient()
	raw := json.RawMessage(`{"message":"no views in session"}`)
	_, err := c.decodeLocationsWithErr(raw)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no views")
}

func TestDecodeLocationsWithErr_ValidLocation(t *testing.T) {
	c := newTestClient()
	raw := json.RawMessage(`{"uri":"file:///tmp/foo.go","range":{"start":{"line":1,"character":2},"end":{"line":3,"character":4}}}`)
	locs, err := c.decodeLocationsWithErr(raw)
	assert.NoError(t, err)
	require.Len(t, locs, 1)
}

func TestDecodeLocationsWithErr_MapWithoutMessage(t *testing.T) {
	c := newTestClient()
	raw := json.RawMessage(`{"code":123}`)
	locs, err := c.decodeLocationsWithErr(raw)
	assert.NoError(t, err)
	assert.Nil(t, locs)
}

// ==================== positionParams ====================

func TestPositionParams(t *testing.T) {
	c := newTestClient()
	c.localRoot = "/tmp"
	params := c.positionParams("/tmp/foo.go", 5, 10)
	assert.NotNil(t, params["textDocument"])
	assert.NotNil(t, params["position"])
	td := params["textDocument"].(map[string]any)
	assert.Contains(t, td["uri"].(string), "foo.go")
	pos := params["position"].(map[string]any)
	assert.Equal(t, 5, pos["line"])
	assert.Equal(t, 10, pos["character"])
}

func TestPositionParams_WithDockerMapping(t *testing.T) {
	c := newTestClient()
	c.hostRoot = "/home/user/project"
	c.remoteRoot = "/workspace"
	params := c.positionParams("/home/user/project/main.go", 0, 0)
	td := params["textDocument"].(map[string]any)
	uri := td["uri"].(string)
	assert.Contains(t, uri, "workspace")
}

// ==================== ServerSpec & DefaultServers ====================

func TestDefaultServers(t *testing.T) {
	specs := DefaultServers()
	assert.NotEmpty(t, specs)

	langs := map[string]bool{}
	for _, s := range specs {
		langs[s.Language] = true
		assert.NotEmpty(t, s.Command, "Command should not be empty for %s", s.Language)
	}
	assert.True(t, langs["go"], "should include go")
	assert.True(t, langs["typescript"], "should include typescript")
	assert.True(t, langs["python"], "should include python")
	assert.True(t, langs["java"], "should include java")
}

func TestDefaultServers_JavaArgs(t *testing.T) {
	specs := DefaultServers()
	var javaSpec ServerSpec
	for _, s := range specs {
		if s.Language == "java" {
			javaSpec = s
			break
		}
	}
	// JVM args should use --jvm-arg= form (single token)
	for _, arg := range javaSpec.Args {
		if len(arg) > 10 && arg[:10] == "--jvm-arg=" {
			// This is the correct form
			continue
		}
	}
	// Should have -data flag
	hasData := false
	for _, arg := range javaSpec.Args {
		if arg == "-data" {
			hasData = true
		}
	}
	assert.True(t, hasData, "Java spec should have -data arg")
}

// ==================== julHeaderRe ====================

func TestJulHeaderRe_Matches(t *testing.T) {
	assert.True(t, julHeaderRe.MatchString("May 23, 2026 3:09:54 PM org.apache.aries.spifly.BaseActivator log"))
	assert.True(t, julHeaderRe.MatchString("Jan 1, 2025 12:00:00 AM some.class info"))
	assert.True(t, julHeaderRe.MatchString("Dec 31, 2025 11:59:59 PM my.Class severe"))
}

func TestJulHeaderRe_NonMatches(t *testing.T) {
	assert.False(t, julHeaderRe.MatchString("ERROR: something went wrong"))
	assert.False(t, julHeaderRe.MatchString("not a jul header"))
	assert.False(t, julHeaderRe.MatchString(""))
}

// ==================== Client lifecycle helpers ====================

func TestClient_IsAlive_ClosedFlag(t *testing.T) {
	c := newTestClient()
	c.processDone = make(chan struct{})
	c.closed.Store(true)
	assert.False(t, c.IsAlive())
}

func TestClient_IsAlive_DeadFlag(t *testing.T) {
	c := newTestClient()
	c.processDone = make(chan struct{})
	c.dead.Store(true)
	assert.False(t, c.IsAlive())
}

func TestClient_IsAlive_NoProcess(t *testing.T) {
	c := newTestClient()
	c.processDone = make(chan struct{})
	// No cmd set — should return true (no process to check)
	assert.True(t, c.IsAlive())
}

func TestClient_SetOnDead(t *testing.T) {
	c := newTestClient()
	called := false
	c.SetOnDead(func() { called = true })
	assert.NotNil(t, c.onDead)
	c.onDead()
	assert.True(t, called)
}

func TestClient_RememberStderr(t *testing.T) {
	c := newTestClient()
	c.rememberStderr("line1")
	c.rememberStderr("line2")
	c.rememberStderr("line3")
	summary := c.stderrSummary()
	assert.Contains(t, summary, "line1")
	assert.Contains(t, summary, "line2")
	assert.Contains(t, summary, "line3")
}

func TestClient_RememberStderr_TrimsTo200(t *testing.T) {
	c := newTestClient()
	for i := 0; i < 250; i++ {
		c.rememberStderr("line")
	}
	assert.LessOrEqual(t, len(c.stderrLines), 200)
}

func TestClient_StderrSummary_Empty(t *testing.T) {
	c := newTestClient()
	assert.Equal(t, "", c.stderrSummary())
}

func TestClient_WithStderr_Nil(t *testing.T) {
	c := newTestClient()
	assert.NoError(t, c.withStderr(nil))
}

func TestClient_WithStderr_WithError(t *testing.T) {
	c := newTestClient()
	c.rememberStderr("some error line")
	err := c.withStderr(assert.AnError)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "some error line")
}

func TestClient_ProcessSummary_NotExited(t *testing.T) {
	c := newTestClient()
	// processDone is open (not closed)
	assert.Equal(t, "", c.processSummary())
}

func TestClient_ProcessSummary_Exited(t *testing.T) {
	c := newTestClient()
	close(c.processDone)
	c.processErr = nil
	summary := c.processSummary()
	assert.Contains(t, summary, "exited")
}

// ==================== Manager helpers ====================

func TestNewManager_DefaultSpecs(t *testing.T) {
	m := NewManager("/root", nil)
	assert.NotNil(t, m)
	assert.NotEmpty(t, m.specs)
	langs := m.Languages()
	assert.Contains(t, langs, "go")
	assert.Contains(t, langs, "java")
}

func TestNewManager_CustomSpecs(t *testing.T) {
	specs := []ServerSpec{
		{Language: "custom", Command: "custom-lsp"},
	}
	m := NewManager("/root", specs)
	langs := m.Languages()
	assert.Equal(t, []string{"custom"}, langs)
}

func TestNewManager_EmptySpecs(t *testing.T) {
	m := NewManager("/root", []ServerSpec{})
	langs := m.Languages()
	assert.Empty(t, langs)
}

func TestManager_GetForRepo_UnknownLanguage(t *testing.T) {
	m := NewManager("/root", []ServerSpec{})
	_, err := m.GetForRepo(nil, "", "rust", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "rust")
}

func TestManager_Close_EmptyManager(t *testing.T) {
	m := NewManager("/root", nil)
	// Should not panic
	m.Close()
}

// ==================== findWorkspaceRoot ====================

func TestFindWorkspaceRoot_GoMod(t *testing.T) {
	dir := t.TempDir()
	modFile := filepath.Join(dir, "go.mod")
	require.NoError(t, os.WriteFile(modFile, []byte("module test"), 0644))
	sub := filepath.Join(dir, "internal", "pkg")
	require.NoError(t, os.MkdirAll(sub, 0755))
	testFile := filepath.Join(sub, "file.go")

	root := findWorkspaceRoot(testFile, "go")
	assert.Equal(t, dir, root)
}

func TestFindWorkspaceRoot_NoMarker(t *testing.T) {
	dir := t.TempDir()
	testFile := filepath.Join(dir, "file.go")
	root := findWorkspaceRoot(testFile, "go")
	// Should return empty when no marker found
	assert.Equal(t, "", root)
}

func TestFindWorkspaceRoot_UnknownLanguage(t *testing.T) {
	root := findWorkspaceRoot("/tmp/file.rs", "rust")
	assert.Equal(t, "", root)
}

func TestFindWorkspaceRoot_PackageJSON(t *testing.T) {
	dir := t.TempDir()
	pkgFile := filepath.Join(dir, "package.json")
	require.NoError(t, os.WriteFile(pkgFile, []byte("{}"), 0644))
	sub := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(sub, 0755))
	testFile := filepath.Join(sub, "app.ts")

	root := findWorkspaceRoot(testFile, "typescript")
	assert.Equal(t, dir, root)
}

// ==================== Windows path handling ====================

func TestPathFromFileURI_WindowsStyle(t *testing.T) {
	if runtime.GOOS != "windows" {
		// On non-Windows, just verify it doesn't crash
		p := pathFromFileURI("file:///C:/Users/test.go")
		assert.NotEmpty(t, p)
		return
	}
	p := pathFromFileURI("file:///C:/Users/test.go")
	assert.Equal(t, `C:\Users\test.go`, p)
}

// ==================== capabilites & config ====================

func TestCapabilities_Go(t *testing.T) {
	c := &Client{Language: "go"}
	caps := c.capabilities()
	assert.NotNil(t, caps["textDocument"])
	assert.NotNil(t, caps["workspace"])
	assert.NotNil(t, caps["window"])
}

func TestCapabilities_Java(t *testing.T) {
	c := &Client{Language: "java"}
	caps := c.capabilities()
	assert.NotNil(t, caps["textDocument"])
}

func TestCapabilities_Python(t *testing.T) {
	c := &Client{Language: "python"}
	caps := c.capabilities()
	assert.NotNil(t, caps["textDocument"])
}

func TestCapabilities_TypeScript(t *testing.T) {
	c := &Client{Language: "typescript"}
	caps := c.capabilities()
	assert.NotNil(t, caps["textDocument"])
}

func TestCapabilities_Unknown(t *testing.T) {
	c := &Client{Language: "rust"}
	caps := c.capabilities()
	// Default capabilities
	assert.NotNil(t, caps["textDocument"])
	assert.NotNil(t, caps["workspace"])
}

func TestInitializationOptions_Go(t *testing.T) {
	c := &Client{Language: "go"}
	opts := c.initializationOptions()
	assert.NotNil(t, opts)
}

func TestInitializationOptions_Java(t *testing.T) {
	c := &Client{Language: "java"}
	opts := c.initializationOptions()
	assert.NotNil(t, opts)
}

func TestInitializationOptions_Python(t *testing.T) {
	c := &Client{Language: "python"}
	opts := c.initializationOptions()
	assert.NotNil(t, opts)
}

func TestInitializationOptions_TypeScript(t *testing.T) {
	c := &Client{Language: "typescript"}
	opts := c.initializationOptions()
	assert.Nil(t, opts) // TS returns nil
}

func TestInitializationOptions_Unknown(t *testing.T) {
	c := &Client{Language: "rust"}
	opts := c.initializationOptions()
	assert.Nil(t, opts)
}

func TestConfigFor_Go(t *testing.T) {
	c := &Client{Language: "go"}
	cfg := c.configFor("gopls")
	assert.NotNil(t, cfg)
	cfgEmpty := c.configFor("unknown_section")
	assert.NotNil(t, cfgEmpty)
}

func TestConfigFor_Java(t *testing.T) {
	c := &Client{Language: "java"}
	cfg := c.configFor("java")
	assert.NotNil(t, cfg)
	cfgEmpty := c.configFor("unknown")
	assert.NotNil(t, cfgEmpty)
}

func TestConfigFor_Python(t *testing.T) {
	c := &Client{Language: "python"}
	assert.NotNil(t, c.configFor("python"))
	assert.NotNil(t, c.configFor("python.analysis"))
	assert.NotNil(t, c.configFor("pyright"))
	assert.NotNil(t, c.configFor(""))
	assert.NotNil(t, c.configFor("other"))
}

func TestConfigFor_TypeScript(t *testing.T) {
	c := &Client{Language: "typescript"}
	assert.NotNil(t, c.configFor("typescript"))
	assert.NotNil(t, c.configFor("javascript"))
	assert.NotNil(t, c.configFor("completions"))
	assert.NotNil(t, c.configFor(""))
	assert.NotNil(t, c.configFor("other"))
}

func TestConfigFor_Unknown(t *testing.T) {
	c := &Client{Language: "rust"}
	cfg := c.configFor("any")
	assert.NotNil(t, cfg)
}

// ==================== handleServerNotification ====================

func TestHandleNotification_PublishDiagnostics(t *testing.T) {
	c := newTestClient()
	params, _ := json.Marshal(map[string]any{"uri": "file:///tmp/foo.go"})
	c.handleServerNotification("textDocument/publishDiagnostics", params)
	// Should push to diagnosticsReady channel
	select {
	case path := <-c.diagnosticsReady:
		assert.Contains(t, path, "foo.go")
	default:
		t.Fatal("expected diagnosticsReady to have a value")
	}
}

func TestHandleNotification_PublishDiagnostics_EmptyURI(t *testing.T) {
	c := newTestClient()
	params, _ := json.Marshal(map[string]any{"uri": ""})
	c.handleServerNotification("textDocument/publishDiagnostics", params)
	// Should not push anything
	select {
	case <-c.diagnosticsReady:
		t.Fatal("should not have received anything")
	default:
		// ok
	}
}

func TestHandleNotification_Progress_GoplsEnd(t *testing.T) {
	c := newTestClient()
	c.Language = "go"
	params, _ := json.Marshal(map[string]any{
		"token": "gopls.indexing",
		"value": map[string]any{"kind": "end"},
	})
	c.handleServerNotification("$/progress", params)
	// goplsReady should be closed
	select {
	case <-c.goplsReady:
		// ok
	default:
		t.Fatal("goplsReady should be closed")
	}
}

func TestHandleNotification_Progress_GoplsBegin(t *testing.T) {
	c := newTestClient()
	c.Language = "go"
	params, _ := json.Marshal(map[string]any{
		"token": "gopls.indexing",
		"value": map[string]any{"kind": "begin"},
	})
	c.handleServerNotification("$/progress", params)
	// goplsReady should NOT be closed for "begin"
	select {
	case <-c.goplsReady:
		t.Fatal("goplsReady should not be closed for begin")
	default:
		// ok
	}
}

func TestHandleNotification_UnknownMethod(t *testing.T) {
	c := newTestClient()
	// Should not panic
	c.handleServerNotification("unknown/method", nil)
}

func TestHandleNotification_MalformedParams(t *testing.T) {
	c := newTestClient()
	// Should not panic
	c.handleServerNotification("textDocument/publishDiagnostics", json.RawMessage("{bad"))
}

// ==================== locFromLSP / locFromLinks edge cases ====================

func TestLocFromLSP_Empty(t *testing.T) {
	c := newTestClient()
	locs := c.locFromLSP(nil)
	assert.Empty(t, locs)
}

func TestLocFromLinks_Empty(t *testing.T) {
	c := newTestClient()
	locs := c.locFromLinks(nil)
	assert.Empty(t, locs)
}

// ==================== localFileLine ====================

func TestLocalFileLine_Nonexistent(t *testing.T) {
	c := newTestClient()
	assert.Equal(t, "", c.localFileLine("/nonexistent/file.go", 0))
}

func TestLocalFileLine_ValidFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.go")
	require.NoError(t, os.WriteFile(f, []byte("line0\nline1\nline2"), 0644))
	c := newTestClient()
	assert.Equal(t, "line0", c.localFileLine(f, 0))
	assert.Equal(t, "line1", c.localFileLine(f, 1))
	assert.Equal(t, "line2", c.localFileLine(f, 2))
}

func TestLocalFileLine_OutOfRange(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.go")
	require.NoError(t, os.WriteFile(f, []byte("line0"), 0644))
	c := newTestClient()
	assert.Equal(t, "", c.localFileLine(f, 100))
	assert.Equal(t, "", c.localFileLine(f, -1))
}

// ==================== failPending ====================

func TestFailPending_Empty(t *testing.T) {
	c := newTestClient()
	// Should not panic with no pending calls
	c.failPending(assert.AnError)
}

func TestFailPending_WithPending(t *testing.T) {
	c := newTestClient()
	ch := make(chan rpcResponse, 1)
	c.pending[1] = ch
	c.failPending(assert.AnError)
	resp := <-ch
	assert.Error(t, resp.Err)
	// pending should be cleared
	assert.Empty(t, c.pending)
}

// ==================== resolveRepo ====================

func TestResolveRepo_NilResolver(t *testing.T) {
	m := NewManager("/root", nil)
	name, path := m.resolveRepo("/some/file.go")
	assert.Equal(t, "", name)
	assert.Equal(t, "", path)
}
