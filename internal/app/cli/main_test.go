package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── isKnownLetters ─────────────────────────────────────────────────────────

func TestIsKnownLetters_AllKnown(t *testing.T) {
	assert.True(t, isKnownLetters("tvlw", "tvlws"))
}

func TestIsKnownLetters_SingleLetter(t *testing.T) {
	assert.True(t, isKnownLetters("t", "tvlws"))
}

func TestIsKnownLetters_DuplicateLetters(t *testing.T) {
	// Duplicate known letters is still "known"
	assert.True(t, isKnownLetters("tt", "tvlws"))
}

func TestIsKnownLetters_UnknownLetter(t *testing.T) {
	assert.False(t, isKnownLetters("txv", "tvlws"))
}

func TestIsKnownLetters_AllUnknown(t *testing.T) {
	assert.False(t, isKnownLetters("abc", "tvlws"))
}

func TestIsKnownLetters_Empty(t *testing.T) {
	assert.True(t, isKnownLetters("", "tvlws"))
}

func TestIsKnownLetters_EmptyKnown(t *testing.T) {
	assert.False(t, isKnownLetters("t", ""))
}

func TestIsKnownLetters_BothEmpty(t *testing.T) {
	assert.True(t, isKnownLetters("", ""))
}

func TestIsKnownLetters_IncludesS(t *testing.T) {
	assert.True(t, isKnownLetters("s", "tvlws"))
	assert.True(t, isKnownLetters("tvlws", "tvlws"))
}

// ─── expandRunShortFlags ────────────────────────────────────────────────────

func TestExpandRunShortFlags_EmptyArgs(t *testing.T) {
	got := expandRunShortFlags(nil)
	assert.Nil(t, got)
}

func TestExpandRunShortFlags_SingleArg(t *testing.T) {
	got := expandRunShortFlags([]string{"ai-tools"})
	assert.Equal(t, []string{"ai-tools"}, got)
}

func TestExpandRunShortFlags_NoRunCommand(t *testing.T) {
	args := []string{"ai-tools", "watch", "-tvlw"}
	got := expandRunShortFlags(args)
	assert.Equal(t, args, got, "should not expand for non-run commands")
}

func TestExpandRunShortFlags_RunSingleFlag(t *testing.T) {
	args := []string{"ai-tools", "run", "-t"}
	got := expandRunShortFlags(args)
	// -t is len 2, not > 2, so it should stay as-is
	assert.Equal(t, []string{"ai-tools", "run", "-t"}, got)
}

func TestExpandRunShortFlags_RunCombinedFlags(t *testing.T) {
	args := []string{"ai-tools", "run", "-tvlw"}
	got := expandRunShortFlags(args)
	assert.Equal(t, []string{"ai-tools", "run", "-t", "-v", "-l", "-w"}, got)
}

func TestExpandRunShortFlags_RunCombinedTwoFlags(t *testing.T) {
	args := []string{"ai-tools", "run", "-tw"}
	got := expandRunShortFlags(args)
	assert.Equal(t, []string{"ai-tools", "run", "-t", "-w"}, got)
}

func TestExpandRunShortFlags_RunWithUnknownLetters(t *testing.T) {
	args := []string{"ai-tools", "run", "-txv"}
	got := expandRunShortFlags(args)
	// Contains 'x' which is unknown → token left as-is
	assert.Equal(t, []string{"ai-tools", "run", "-txv"}, got)
}

func TestExpandRunShortFlags_RunWithPositionalArg(t *testing.T) {
	args := []string{"ai-tools", "run", "-tvlw", "."}
	got := expandRunShortFlags(args)
	assert.Equal(t, []string{"ai-tools", "run", "-t", "-v", "-l", "-w", "."}, got)
}

func TestExpandRunShortFlags_RunWithConfigFlag(t *testing.T) {
	args := []string{"ai-tools", "--config", "my.yaml", "run", "-tw"}
	got := expandRunShortFlags(args)
	assert.Equal(t, []string{"ai-tools", "--config", "my.yaml", "run", "-t", "-w"}, got)
}

func TestExpandRunShortFlags_RunWithShortConfigFlag(t *testing.T) {
	args := []string{"ai-tools", "-c", "my.yaml", "run", "-vl"}
	got := expandRunShortFlags(args)
	assert.Equal(t, []string{"ai-tools", "-c", "my.yaml", "run", "-v", "-l"}, got)
}

func TestExpandRunShortFlags_RunWithConfigEquals(t *testing.T) {
	args := []string{"ai-tools", "--config=my.yaml", "run", "-tw"}
	got := expandRunShortFlags(args)
	assert.Equal(t, []string{"ai-tools", "--config=my.yaml", "run", "-t", "-w"}, got)
}

func TestExpandRunShortFlags_RunWithShortConfigEquals(t *testing.T) {
	args := []string{"ai-tools", "-c=my.yaml", "run", "-lv"}
	got := expandRunShortFlags(args)
	assert.Equal(t, []string{"ai-tools", "-c=my.yaml", "run", "-l", "-v"}, got)
}

func TestExpandRunShortFlags_DoubleDashNotExpanded(t *testing.T) {
	args := []string{"ai-tools", "run", "--ts"}
	got := expandRunShortFlags(args)
	// Starts with -- → not a short flag combo
	assert.Equal(t, []string{"ai-tools", "run", "--ts"}, got)
}

func TestExpandRunShortFlags_MixedFlagsAndArgs(t *testing.T) {
	args := []string{"ai-tools", "run", "-tvlw", "/some/dir", "--no-tui"}
	got := expandRunShortFlags(args)
	assert.Equal(t, []string{"ai-tools", "run", "-t", "-v", "-l", "-w", "/some/dir", "--no-tui"}, got)
}

func TestExpandRunShortFlags_RunWithSFlag(t *testing.T) {
	args := []string{"ai-tools", "run", "-tvlws"}
	got := expandRunShortFlags(args)
	assert.Equal(t, []string{"ai-tools", "run", "-t", "-v", "-l", "-w", "-s"}, got)
}

func TestExpandRunShortFlags_NonRunFirstArg(t *testing.T) {
	args := []string{"ai-tools", "watch"}
	got := expandRunShortFlags(args)
	// "watch" is not "run", loop breaks
	assert.Equal(t, args, got)
}

func TestExpandRunShortFlags_LongFlagBeforeRun(t *testing.T) {
	// A random flag that is not --config or -c before "run"
	args := []string{"ai-tools", "--verbose", "run", "-tw"}
	got := expandRunShortFlags(args)
	// "--verbose" is neither --config/-c nor "run", so the loop breaks
	// and runIdx stays -1
	assert.Equal(t, args, got)
}

func TestExpandRunShortFlags_OnlyDashes(t *testing.T) {
	// Token with just "-" prefix and known letters
	args := []string{"ai-tools", "run", "-tvl"}
	got := expandRunShortFlags(args)
	assert.Equal(t, []string{"ai-tools", "run", "-t", "-v", "-l"}, got)
}

// ─── removeIfExists ─────────────────────────────────────────────────────────

func TestRemoveIfExists_FileExists(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	err := removeIfExists(path)
	assert.NoError(t, err)
	assert.NoFileExists(t, path)
}

func TestRemoveIfExists_FileNotExist(t *testing.T) {
	err := removeIfExists(filepath.Join(t.TempDir(), "nonexistent"))
	assert.NoError(t, err)
}

func TestRemoveIfExists_Directory(t *testing.T) {
	// os.Remove on non-empty dir should fail
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "file.txt"), []byte("x"), 0o644))

	err := removeIfExists(subDir)
	// Non-empty directory: os.Remove returns error
	assert.Error(t, err)
}

// ─── getLSPCommand ──────────────────────────────────────────────────────────

func TestGetLSPCommand(t *testing.T) {
	tests := []struct {
		lang string
		want string
	}{
		{"go", "gopls"},
		{"typescript", "typescript-language-server"},
		{"javascript", "typescript-language-server"},
		{"python", "pyright-langserver"},
		{"java", "jdtls"},
		{"rust", "rust-language-server"},
		{"unknown", "unknown-language-server"},
	}
	for _, tc := range tests {
		t.Run(tc.lang, func(t *testing.T) {
			got := getLSPCommand(tc.lang)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ─── getLSPArgs ─────────────────────────────────────────────────────────────

func TestGetLSPArgs(t *testing.T) {
	tests := []struct {
		lang     string
		wantLen  int
		wantElem string // first element to check (if any)
	}{
		{"typescript", 1, "--stdio"},
		{"javascript", 1, "--stdio"},
		{"python", 1, "--stdio"},
		{"java", -1, "--jvm-arg=-Xmx4G"}, // just check first element, variable length
		{"go", 0, ""},
		{"rust", 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.lang, func(t *testing.T) {
			got := getLSPArgs(tc.lang)
			if tc.wantLen >= 0 {
				assert.Len(t, got, tc.wantLen)
			} else {
				assert.NotEmpty(t, got)
			}
			if tc.wantElem != "" {
				require.NotEmpty(t, got)
				assert.Equal(t, tc.wantElem, got[0])
			}
		})
	}
}

func TestGetLSPArgs_JavaContainsDataDir(t *testing.T) {
	args := getLSPArgs("java")
	found := false
	for _, a := range args {
		if a == "-data" {
			found = true
		}
	}
	assert.True(t, found, "java args should contain -data flag")
}

// ─── cobra command structure ────────────────────────────────────────────────

func TestNewRunCmd_Flags(t *testing.T) {
	cmd := newRunCmd()
	assert.NotNil(t, cmd)

	// Check that expected flags exist
	flags := []string{"vec", "lsp", "sym", "watch", "env", "no-tui"}
	for _, name := range flags {
		f := cmd.Flags().Lookup(name)
		assert.NotNil(t, f, "flag %q should exist", name)
	}

	// Check short flags
	shortFlags := map[string]string{"vec": "v", "lsp": "l", "sym": "s", "watch": "w"}
	for name, short := range shortFlags {
		f := cmd.Flags().ShorthandLookup(short)
		assert.NotNil(t, f, "short flag -%s for %q should exist", short, name)
	}
}

func TestNewRunCmd_NothingToRun(t *testing.T) {
	cmd := newRunCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	// Execute with no flags — should error "nothing to run"
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to run")
}

func TestNewRunCmd_InvalidEnv(t *testing.T) {
	cmd := newRunCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--env", "invalid", "-v"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --env:")
}

func TestNewWatchCmd_Flags(t *testing.T) {
	cmd := newWatchCmd()
	assert.NotNil(t, cmd)

	flags := []string{"env", "skip-vector", "no-tui"}
	for _, name := range flags {
		f := cmd.Flags().Lookup(name)
		assert.NotNil(t, f, "flag %q should exist", name)
	}
}

func TestNewCleanCmd_Flags(t *testing.T) {
	cmd := newCleanCmd()
	assert.NotNil(t, cmd)

	flags := []string{"all", "skip-qdrant", "skip-sqlite", "storages"}
	for _, name := range flags {
		f := cmd.Flags().Lookup(name)
		assert.NotNil(t, f, "flag %q should exist", name)
	}
}

func TestNewGenConfigCmd_Flags(t *testing.T) {
	cmd := newGenConfigCmd()
	assert.NotNil(t, cmd)

	flags := []string{"path", "local", "force"}
	for _, name := range flags {
		f := cmd.Flags().Lookup(name)
		assert.NotNil(t, f, "flag %q should exist", name)
	}
}

func TestNewMcpConfigCmd_Flags(t *testing.T) {
	cmd := newMcpConfigCmd()
	assert.NotNil(t, cmd)

	f := cmd.Flags().Lookup("mode")
	assert.NotNil(t, f)
	assert.Equal(t, "sse", f.DefValue)

	f = cmd.Flags().Lookup("root")
	assert.NotNil(t, f)
}

func TestNewServeCmd_HasRootFlag(t *testing.T) {
	cmd := newServeCmd()
	f := cmd.Flags().Lookup("root")
	assert.NotNil(t, f)
}

// rootCmd subcommands are added in main(), not at init, so not testable via unit test.

// ─── addRootFlag helper ────────────────────────────────────────────────────

func TestAddRootFlag(t *testing.T) {
	cmd := newCleanCmd() // just any command
	var root string
	addRootFlag(cmd, &root)
	f := cmd.Flags().Lookup("root")
	assert.NotNil(t, f)
	assert.Equal(t, ".", f.DefValue)
}
