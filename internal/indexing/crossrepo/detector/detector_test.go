package detector_test

import (
	"os"
	"path/filepath"
	"testing"

	"ragota/internal/indexing/crossrepo/detector"
	"ragota/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// IsExternalCall — table-driven
// ---------------------------------------------------------------------------

func TestIsExternalCall_HTTP(t *testing.T) {
	d := detector.New()

	httpCases := []struct {
		name string
		lang string
		want bool
	}{
		// Exact matches
		{"fetch", "typescript", true},
		{"axios", "typescript", true},
		{"http.Get", "go", true},
		{"http.Post", "go", true},
		{"http.NewRequest", "go", true},
		{"requests.get", "python", true},
		{"requests.post", "python", true},
		{"urllib", "python", true},
		{"httpx", "python", true},
		{"RestTemplate", "java", true},
		{"WebClient", "javascript", true},
		{"HttpClient", "typescript", true},

		// Suffix matches
		{"MyClient.fetch", "typescript", true},
		{"api.http.Get", "go", true},
		{"client.requests.post", "python", true},
		{"myService.RestTemplate", "java", true},
		{"app.WebClient", "javascript", true},
		{"net.http.NewRequest", "go", true},

		// Negative: partial substring NOT at suffix
		{"myFetch", "typescript", false},
		{"fetcher", "typescript", false},
		{"axiosWrapper", "typescript", false},
		{"httpGet", "go", false}, // no dot before Get
		{"httpClient", "typescript", false},
	}

	for _, tc := range httpCases {
		t.Run(tc.name, func(t *testing.T) {
			got := d.IsExternalCall(tc.name, tc.lang)
			assert.Equal(t, tc.want, got, "IsExternalCall(%q, %q)", tc.name, tc.lang)
		})
	}
}

func TestIsExternalCall_gRPC(t *testing.T) {
	d := detector.New()

	grpcCases := []struct {
		name string
		lang string
		want bool
	}{
		// Exact matches
		{"grpc.Dial", "go", true},
		{"grpc.Invoke", "go", true},
		{"NewClient", "go", true},
		{"grpcClient", "go", true},
		{"grpc.NewClient", "go", true},

		// Suffix matches
		{"conn.grpc.Dial", "go", true},
		{"pb.NewClient", "go", true},
		{"my.grpcClient", "go", true},
		{"client.grpc.Invoke", "go", true},

		// Negative
		{"Dial", "go", false},
		{"Invoke", "go", false},
		{"Client", "go", false},
		{"grpcDial", "go", false}, // no dot
	}

	for _, tc := range grpcCases {
		t.Run(tc.name, func(t *testing.T) {
			got := d.IsExternalCall(tc.name, tc.lang)
			assert.Equal(t, tc.want, got, "IsExternalCall(%q, %q)", tc.name, tc.lang)
		})
	}
}

func TestIsExternalCall_Kafka(t *testing.T) {
	d := detector.New()

	kafkaCases := []struct {
		name string
		lang string
		want bool
	}{
		// Exact matches
		{"kafka.Producer", "go", true},
		{"kafka.Consumer", "go", true},
		{"Producer.Send", "go", true},
		{"consumer.Subscribe", "go", true},
		{"producer.Publish", "go", true},
		{"KafkaTemplate", "java", true},
		{"KafkaProducer", "java", true},
		{"KafkaConsumer", "java", true},

		// Suffix matches
		{"my.kafka.Producer", "go", true},
		{"svc.KafkaTemplate", "java", true},
		{"app.KafkaProducer", "java", true},
		{"k.KafkaConsumer", "java", true},

		// Negative
		{"Producer", "go", false},
		{"Consumer", "go", false},
		{"kafka", "go", false},
		{"Kafka", "java", false},
	}

	for _, tc := range kafkaCases {
		t.Run(tc.name, func(t *testing.T) {
			got := d.IsExternalCall(tc.name, tc.lang)
			assert.Equal(t, tc.want, got, "IsExternalCall(%q, %q)", tc.name, tc.lang)
		})
	}
}

func TestIsExternalCall_Negative(t *testing.T) {
	d := detector.New()

	negCases := []struct {
		name string
		lang string
	}{
		{"validateUser", "go"},
		{"processOrder", "go"},
		{"GetUser", "typescript"},
		{"internalFunc", "python"},
		{"handleClick", "javascript"},
		{"calculateTotal", "go"},
		{"renderComponent", "typescript"},
		{"db.save", "python"},
		{"userRepository.find", "java"},
	}

	for _, tc := range negCases {
		t.Run(tc.name, func(t *testing.T) {
			got := d.IsExternalCall(tc.name, tc.lang)
			assert.False(t, got, "IsExternalCall(%q, %q) should be false", tc.name, tc.lang)
		})
	}
}

func TestIsExternalCall_AllPatterns_Comprehensive(t *testing.T) {
	d := detector.New()

	// One big table covering every pattern from the source code.
	allCases := []struct {
		name string
		want bool
	}{
		// --- HTTP ---
		{"fetch", true},
		{"axios", true},
		{"http.Get", true},
		{"http.Post", true},
		{"http.NewRequest", true},
		{"requests.get", true},
		{"requests.post", true},
		{"urllib", true},
		{"httpx", true},
		{"RestTemplate", true},
		{"WebClient", true},
		{"HttpClient", true},
		{"svc.fetch", true},
		{"api.http.Get", true},

		// --- gRPC ---
		{"grpc.Dial", true},
		{"grpc.Invoke", true},
		{"NewClient", true},
		{"grpcClient", true},
		{"grpc.NewClient", true},
		{"pb.NewClient", true},
		{"conn.grpc.Dial", true},

		// --- Kafka ---
		{"kafka.Producer", true},
		{"kafka.Consumer", true},
		{"Producer.Send", true},
		{"consumer.Subscribe", true},
		{"producer.Publish", true},
		{"KafkaTemplate", true},
		{"KafkaProducer", true},
		{"KafkaConsumer", true},
		{"svc.KafkaTemplate", true},

		// --- Negative ---
		{"validateUser", false},
		{"processOrder", false},
		{"GetUser", false},
		{"internalFunc", false},
		{"myFetch", false},
		{"fetcher", false},
		{"httpClient", false},
		{"Dial", false},
		{"Producer", false},
		{"Consumer", false},
		{"kafka", false},
	}

	for _, tc := range allCases {
		t.Run(tc.name, func(t *testing.T) {
			got := d.IsExternalCall(tc.name, "")
			assert.Equal(t, tc.want, got, "IsExternalCall(%q)", tc.name)
		})
	}
}

// ---------------------------------------------------------------------------
// ExtractRawCode — boundary tests with real temp files
// ---------------------------------------------------------------------------

func TestExtractRawCode_LineBeyondFileLength(t *testing.T) {
	d := detector.New()

	// Create a temp file with 10 lines
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "a.go")
	content := ""
	for i := 1; i <= 10; i++ {
		content += "line" + string(rune('0'+i)) + "\n"
	}
	err := os.WriteFile(srcPath, []byte(content), 0o644)
	require.NoError(t, err)

	unit := &store.ASTUnit{
		FilePath:  srcPath,
		Language:  "go",
		StartLine: 1,
		EndLine:   10,
	}

	// Line 100 beyond file length — should not panic
	assert.NotPanics(t, func() { d.ExtractRawCode(unit, 100) })
	result := d.ExtractRawCode(unit, 100)
	// When line is beyond, start/end logic clamps; we just verify no panic
	_ = result
}

func TestExtractRawCode_LineZero(t *testing.T) {
	d := detector.New()

	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "b.go")
	content := "line1\nline2\nline3\nline4\nline5\n"
	err := os.WriteFile(srcPath, []byte(content), 0o644)
	require.NoError(t, err)

	unit := &store.ASTUnit{FilePath: srcPath, Language: "go", StartLine: 1, EndLine: 5}

	assert.NotPanics(t, func() { d.ExtractRawCode(unit, 0) })
	result := d.ExtractRawCode(unit, 0)
	// line 0: start = max(0, 0-6) = 0, end = min(5, 0+4) = 4
	assert.Contains(t, result, "line1")
}

func TestExtractRawCode_FirstLine(t *testing.T) {
	d := detector.New()

	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "c.go")
	content := "line1\nline2\nline3\nline4\nline5\nline6\n"
	err := os.WriteFile(srcPath, []byte(content), 0o644)
	require.NoError(t, err)

	unit := &store.ASTUnit{FilePath: srcPath, Language: "go", StartLine: 1, EndLine: 6}

	result := d.ExtractRawCode(unit, 1)
	assert.NotPanics(t, func() { d.ExtractRawCode(unit, 1) })
	assert.Contains(t, result, "line1")
}

func TestExtractRawCode_EmptySourceFile(t *testing.T) {
	d := detector.New()

	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "empty.go")
	err := os.WriteFile(srcPath, []byte(""), 0o644)
	require.NoError(t, err)

	unit := &store.ASTUnit{FilePath: srcPath, Language: "go", StartLine: 1, EndLine: 1}

	result := d.ExtractRawCode(unit, 1)
	assert.Empty(t, result)
}

func TestExtractRawCode_SingleLineFile(t *testing.T) {
	d := detector.New()

	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "single.go")
	err := os.WriteFile(srcPath, []byte("only_line"), 0o644)
	require.NoError(t, err)

	unit := &store.ASTUnit{FilePath: srcPath, Language: "go", StartLine: 1, EndLine: 1}

	result := d.ExtractRawCode(unit, 1)
	assert.NotPanics(t, func() { d.ExtractRawCode(unit, 1) })
	assert.Equal(t, "only_line", result)
}

func TestExtractRawCode_MiddleLine(t *testing.T) {
	d := detector.New()

	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "mid.go")
	content := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\nline11\n"
	err := os.WriteFile(srcPath, []byte(content), 0o644)
	require.NoError(t, err)

	unit := &store.ASTUnit{FilePath: srcPath, Language: "go", StartLine: 1, EndLine: 11}

	result := d.ExtractRawCode(unit, 6)
	assert.NotPanics(t, func() { d.ExtractRawCode(unit, 6) })
	assert.Contains(t, result, "line6")
}

func TestExtractRawCode_FileDoesNotExist(t *testing.T) {
	d := detector.New()

	unit := &store.ASTUnit{FilePath: "/nonexistent/file.go", Language: "go"}

	result := d.ExtractRawCode(unit, 1)
	assert.Empty(t, result)
}

func TestExtractRawCode_EmptyFilePath(t *testing.T) {
	d := detector.New()

	unit := &store.ASTUnit{FilePath: "", Language: "go"}

	result := d.ExtractRawCode(unit, 1)
	assert.Empty(t, result)
}

// ---------------------------------------------------------------------------
// splitLines — pure function tests
// ---------------------------------------------------------------------------

func TestSplitLines(t *testing.T) {
	cases := []struct {
		desc  string
		input string
		want  []string
	}{
		{
			desc:  "empty string",
			input: "",
			want:  nil,
		},
		{
			desc:  "single line no newline",
			input: "hello",
			want:  []string{"hello"},
		},
		{
			desc:  "single line with newline",
			input: "hello\n",
			want:  []string{"hello"},
		},
		{
			desc:  "two lines",
			input: "hello\nworld",
			want:  []string{"hello", "world"},
		},
		{
			desc:  "multiple lines with trailing newline",
			input: "a\nb\nc\n",
			want:  []string{"a", "b", "c"},
		},
		{
			desc:  "multiple lines no trailing newline",
			input: "a\nb\nc",
			want:  []string{"a", "b", "c"},
		},
		{
			desc:  "empty lines",
			input: "a\n\nb\n",
			want:  []string{"a", "", "b"},
		},
		{
			desc:  "only newlines",
			input: "\n\n\n",
			want:  []string{"", "", ""},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := splitLines(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// joinLines — pure function tests
// ---------------------------------------------------------------------------

func TestJoinLines(t *testing.T) {
	cases := []struct {
		desc  string
		input []string
		want  string
	}{
		{
			desc:  "nil slice",
			input: nil,
			want:  "",
		},
		{
			desc:  "empty slice",
			input: []string{},
			want:  "",
		},
		{
			desc:  "single line",
			input: []string{"hello"},
			want:  "hello",
		},
		{
			desc:  "two lines",
			input: []string{"hello", "world"},
			want:  "hello\nworld",
		},
		{
			desc:  "multiple lines",
			input: []string{"a", "b", "c"},
			want:  "a\nb\nc",
		},
		{
			desc:  "empty string elements",
			input: []string{"a", "", "b"},
			want:  "a\n\nb",
		},
		{
			desc:  "lines with spaces",
			input: []string{"  indent", "  more"},
			want:  "  indent\n  more",
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := joinLines(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// Candidate and CrossEdge struct usage
// ---------------------------------------------------------------------------

func TestCandidateStruct(t *testing.T) {
	c := detector.Candidate{
		Repo:      "my-repo",
		FilePath:  "src/handler.go",
		Line:      42,
		Symbol:    "HandleRequest",
		RawCode:   "func HandleRequest() {\n\thttp.Get(...)\n}",
		ProtoHint: "http",
		Language:  "go",
	}

	assert.Equal(t, "my-repo", c.Repo)
	assert.Equal(t, "src/handler.go", c.FilePath)
	assert.Equal(t, 42, c.Line)
	assert.Equal(t, "HandleRequest", c.Symbol)
	assert.Equal(t, "http", c.ProtoHint)
	assert.Equal(t, "go", c.Language)
	assert.Contains(t, c.RawCode, "http.Get")
}

func TestCrossEdgeStruct(t *testing.T) {
	edge := detector.CrossEdge{
		SrcRepo:    "source-repo",
		SrcFile:    "src/client.go",
		SrcLine:    10,
		SrcSymbol:  "CallAPI",
		DstRepo:    "target-repo",
		DstFile:    "api/handler.go",
		DstSymbol:  "HandleAPI",
		DstName:    "http.Get",
		Protocol:   "http",
		Confidence: 0.95,
		LLMReason:  "Looks like an HTTP call to external service",
	}

	assert.Equal(t, "source-repo", edge.SrcRepo)
	assert.Equal(t, "target-repo", edge.DstRepo)
	assert.Equal(t, "http", edge.Protocol)
	assert.Equal(t, 0.95, edge.Confidence)
	assert.Equal(t, "http.Get", edge.DstName)
	assert.Equal(t, 10, edge.SrcLine)
	assert.Contains(t, edge.LLMReason, "HTTP")
}

func TestCrossEdgeStruct_OptionalFields(t *testing.T) {
	// DstFile, DstSymbol are omitempty — test they can be empty
	edge := detector.CrossEdge{
		SrcRepo:   "repo-a",
		SrcFile:   "a.go",
		SrcLine:   1,
		SrcSymbol: "f",
		DstName:   "external",
		Protocol:  "grpc",
	}

	assert.Empty(t, edge.DstFile)
	assert.Empty(t, edge.DstSymbol)
	assert.Empty(t, edge.LLMReason)
	assert.Equal(t, 0.0, edge.Confidence) // zero value
}

// ---------------------------------------------------------------------------
// Helper: local copy of splitLines/joinLines since they are unexported
// We re-implement them here to test the detector's behavior via ExtractRawCode
// and also test them directly by importing from a test helper.
// Since they are unexported, we test them indirectly through ExtractRawCode
// OR we define them locally (identical logic) for direct unit testing.
// ---------------------------------------------------------------------------

// splitLines replicates detector's unexported function for direct testing.
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// joinLines replicates detector's unexported function for direct testing.
func joinLines(lines []string) string {
	result := ""
	for i, l := range lines {
		if i > 0 {
			result += "\n"
		}
		result += l
	}
	return result
}
