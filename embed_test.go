package ragota

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embeds exist so the binary can re-emit repository files; the one way
// that promise breaks is the embed directives drifting from the tree — a
// fourth skill added on disk but not in embed.go would ship binaries that
// silently install three. These tests pin byte equality in both directions.

func TestConfigExampleMatchesDisk(t *testing.T) {
	disk, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	if !bytes.Equal(disk, ConfigExample) {
		t.Fatal("embedded config.example.yaml differs from the file on disk")
	}
}

func TestSkillsMatchDisk(t *testing.T) {
	// Everything embedded exists on disk with the same bytes.
	embedded := map[string]bool{}
	err := fs.WalkDir(Skills, "skills", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		embedded[p] = true
		got, err := Skills.ReadFile(p)
		if err != nil {
			return err
		}
		want, err := os.ReadFile(filepath.FromSlash(p))
		if err != nil {
			t.Errorf("embedded %s has no counterpart on disk: %v", p, err)
			return nil
		}
		if !bytes.Equal(got, want) {
			t.Errorf("embedded %s differs from the file on disk", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded skills: %v", err)
	}

	// Every skill file on disk is embedded. skills/README.md is about the
	// repository and stays out on purpose.
	entries, err := os.ReadDir("skills")
	if err != nil {
		t.Fatalf("read skills/: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "ragota-") {
			continue
		}
		root := filepath.Join("skills", e.Name())
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !embedded[filepath.ToSlash(p)] {
				t.Errorf("%s exists on disk but is not embedded — extend the //go:embed directive in embed.go", p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
