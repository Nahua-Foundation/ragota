package logger

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Log() returns non-nil logger
// ---------------------------------------------------------------------------

func TestLog_ReturnsNonNil(t *testing.T) {
	l := Log()
	require.NotNil(t, l)
}

func TestLog_ReturnsSamePointer(t *testing.T) {
	l1 := Log()
	l2 := Log()
	assert.Equal(t, l1, l2)
}

// ---------------------------------------------------------------------------
// InitLogger — various levels and formats
// ---------------------------------------------------------------------------

func TestInitLogger_ValidLevels(t *testing.T) {
	levels := []string{"trace", "debug", "info", "warn", "error", "fatal", "panic"}
	for _, level := range levels {
		t.Run(level, func(t *testing.T) {
			var buf bytes.Buffer
			InitLogger(level, false, &buf)
			l := Log()
			require.NotNil(t, l)

			// Parse expected level and check
			expected, err := zerolog.ParseLevel(level)
			require.NoError(t, err)
			assert.Equal(t, expected, l.GetLevel())
		})
	}
}

func TestInitLogger_InvalidLevel_DefaultsToInfo(t *testing.T) {
	var buf bytes.Buffer
	InitLogger("invalid-level", false, &buf)
	l := Log()
	assert.Equal(t, zerolog.InfoLevel, l.GetLevel())
}

func TestInitLogger_EmptyLevel_NoLevel(t *testing.T) {
	// zerolog.ParseLevel("") returns (NoLevel, nil) — empty string is parsed
	// as NoLevel (not an error), so the global logger uses NoLevel.
	var buf bytes.Buffer
	InitLogger("", false, &buf)
	l := Log()
	assert.Equal(t, zerolog.NoLevel, l.GetLevel())
}

func TestInitLogger_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	InitLogger("debug", true, &buf)
	l := Log()
	require.NotNil(t, l)

	l.Info().Msg("test json")
	output := buf.String()
	assert.Contains(t, output, `"level":"info"`)
	assert.Contains(t, output, `"message":"test json"`)
}

func TestInitLogger_ConsoleFormat(t *testing.T) {
	var buf bytes.Buffer
	InitLogger("debug", false, &buf)
	l := Log()
	require.NotNil(t, l)

	l.Info().Msg("test console")
	output := buf.String()
	// Console writer uses ANSI colors and human-readable format
	assert.Contains(t, output, "test console")
}

func TestInitLogger_NilWriter_UsesStderr(t *testing.T) {
	// Should not panic with nil writer
	InitLogger("info", false, nil)
	l := Log()
	require.NotNil(t, l)
	assert.Equal(t, zerolog.InfoLevel, l.GetLevel())
}

// ---------------------------------------------------------------------------
// InitLogger — level filtering
// ---------------------------------------------------------------------------

func TestInitLogger_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	InitLogger("warn", true, &buf)
	l := Log()

	l.Debug().Msg("should not appear")
	l.Info().Msg("should not appear")
	assert.Empty(t, buf.String())

	l.Warn().Msg("warning message")
	assert.Contains(t, buf.String(), "warning message")
}

func TestInitLogger_TraceLevelShowsAll(t *testing.T) {
	var buf bytes.Buffer
	InitLogger("trace", true, &buf)
	l := Log()

	l.Trace().Msg("trace msg")
	assert.Contains(t, buf.String(), "trace msg")
}

// ---------------------------------------------------------------------------
// OpenLogFile
// ---------------------------------------------------------------------------

func TestOpenLogFile_CreatesFileAndDirectory(t *testing.T) {
	root := t.TempDir()
	f, path, err := OpenLogFile(root)
	require.NoError(t, err)
	require.NotNil(t, f)
	defer f.Close()

	expectedPath := root + "/.ragota/log/app.log"
	assert.Equal(t, expectedPath, path)

	// File should exist
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, info.IsDir())

	// Directory should have been created
	logDir := filepath.Join(root, ".ragota", "log")
	dirInfo, err := os.Stat(logDir)
	require.NoError(t, err)
	assert.True(t, dirInfo.IsDir())
}

func TestOpenLogFile_AppendMode(t *testing.T) {
	root := t.TempDir()

	// First open and write
	f1, _, err := OpenLogFile(root)
	require.NoError(t, err)
	_, err = f1.Write([]byte("line1\n"))
	require.NoError(t, err)
	f1.Close()

	// Second open should append
	f2, path, err := OpenLogFile(root)
	require.NoError(t, err)
	defer f2.Close()

	_, err = f2.Write([]byte("line2\n"))
	require.NoError(t, err)

	// Read file and check both lines
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(content), "line1\n")
	assert.Contains(t, string(content), "line2\n")
}

func TestOpenLogFile_WritableFile(t *testing.T) {
	root := t.TempDir()
	f, _, err := OpenLogFile(root)
	require.NoError(t, err)
	defer f.Close()

	n, err := f.Write([]byte("test write"))
	require.NoError(t, err)
	assert.Equal(t, 10, n)
}

func TestOpenLogFile_InvalidRoot(t *testing.T) {
	// Use a path that cannot be created (on macOS/Linux, /dev/null is a file, not dir)
	_, _, err := OpenLogFile("/dev/null/impossible")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Global logger state — InitLogger reconfigures
// ---------------------------------------------------------------------------

func TestInitLogger_Reconfigurable(t *testing.T) {
	var buf1 bytes.Buffer
	InitLogger("error", true, &buf1)
	assert.Equal(t, zerolog.ErrorLevel, Log().GetLevel())

	var buf2 bytes.Buffer
	InitLogger("debug", false, &buf2)
	assert.Equal(t, zerolog.DebugLevel, Log().GetLevel())
}

// ---------------------------------------------------------------------------
// Logging with structured fields
// ---------------------------------------------------------------------------

func TestInitLogger_StructuredFields(t *testing.T) {
	var buf bytes.Buffer
	InitLogger("debug", true, &buf)
	l := Log()

	l.Info().Str("component", "test").Int("count", 42).Msg("structured")
	output := buf.String()
	assert.Contains(t, output, `"component":"test"`)
	assert.Contains(t, output, `"count":42`)
	assert.Contains(t, output, `"message":"structured"`)
}
