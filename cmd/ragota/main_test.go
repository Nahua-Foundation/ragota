package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The subcommand is optional. Every invocation that predates it passes no
// positional argument at all, so the empty case is the one that must not become
// an error — and every word after it is accounted for rather than ignored.
func TestParseCommand(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantName string
		wantSub  reposArgs
		wantErr  bool
	}{
		{name: "no subcommand keeps working", args: nil, wantName: commandRun},
		{name: "the run subcommand", args: []string{"run"}, wantName: commandRun},
		{name: "an unknown command is rejected", args: []string{"serve"}, wantErr: true},
		{name: "a typo is rejected rather than ignored", args: []string{"rnu"}, wantErr: true},
		{name: "trailing arguments are rejected", args: []string{"run", "extra"}, wantErr: true},
		{name: "a stray argument without a command", args: []string{"./dir"}, wantErr: true},
		{
			name: "repos list", args: []string{"repos", "list"},
			wantName: commandRepos, wantSub: reposArgs{action: reposList},
		},
		{
			name: "repos activate names a repository", args: []string{"repos", "activate", "alpha"},
			wantName: commandRepos, wantSub: reposArgs{action: reposActivate, ref: "alpha"},
		},
		{
			name: "repos deactivate names a repository", args: []string{"repos", "deactivate", "~/code/beta"},
			wantName: commandRepos, wantSub: reposArgs{action: reposDeactivate, ref: "~/code/beta"},
		},
		{name: "repos on its own says what it needs", args: []string{"repos"}, wantErr: true},
		{name: "an unknown repos action is rejected", args: []string{"repos", "lsit"}, wantErr: true},
		{name: "repos list takes no argument", args: []string{"repos", "list", "alpha"}, wantErr: true},
		{name: "repos activate needs a repository", args: []string{"repos", "activate"}, wantErr: true},
		{name: "repos activate takes one", args: []string{"repos", "activate", "a", "b"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCommand(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseCommand(%v) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.name != tt.wantName || got.repos != tt.wantSub {
				t.Errorf("parseCommand(%v) = %q %+v, want %q %+v",
					tt.args, got.name, got.repos, tt.wantName, tt.wantSub)
			}
		})
	}
}

// A --source run with no config file anywhere is the headline invocation, and
// it has to produce a usable configuration rather than an error about a file
// nobody asked for.
func TestLoadConfigFallsBackToTheLocalProfile(t *testing.T) {
	t.Setenv("RAGOTA_CONFIG", "")
	t.Chdir(t.TempDir()) // no config.yaml here

	cfg, err := loadConfig("", true)
	if err != nil {
		t.Fatalf("loadConfig() error = %v, want the built-in profile", err)
	}
	if cfg.Storage.SQLite == nil {
		t.Error("the local profile configured no relational storage")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the local profile does not validate: %v", err)
	}
}

// Without --source the missing file is still an error: a server started with
// no configuration is not something to guess at.
func TestLoadConfigWithoutSourceStillRequiresAFile(t *testing.T) {
	t.Setenv("RAGOTA_CONFIG", "")
	t.Chdir(t.TempDir())

	if _, err := loadConfig("", false); err == nil {
		t.Error("loadConfig() with no file and no --source returned no error")
	}
}

// A file the user named and that is not there stays a hard error even in a
// --source run: the fallback covers the path nobody chose, not a typo in one
// somebody did.
func TestLoadConfigNamedFileMustExist(t *testing.T) {
	t.Setenv("RAGOTA_CONFIG", "")
	missing := filepath.Join(t.TempDir(), "nope.yaml")

	if _, err := loadConfig(missing, true); err == nil {
		t.Error("loadConfig() with an explicit missing --config returned no error")
	}

	t.Setenv("RAGOTA_CONFIG", missing)
	if _, err := loadConfig("", true); err == nil {
		t.Error("loadConfig() with an explicit missing RAGOTA_CONFIG returned no error")
	}
}

// A config file that exists is read even in a --source run; the profile is a
// fallback, never an override.
func TestLoadConfigPrefersTheFile(t *testing.T) {
	t.Setenv("RAGOTA_CONFIG", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "storage:\n  sqlite:\n    path: /tmp/from-the-file.db\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path, true)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.Storage.SQLite == nil || cfg.Storage.SQLite.Path != "/tmp/from-the-file.db" {
		t.Errorf("sqlite path = %+v, want the value from the file", cfg.Storage.SQLite)
	}
}
