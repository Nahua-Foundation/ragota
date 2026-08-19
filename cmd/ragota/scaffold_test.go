package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	assets "github.com/Nahua-Foundation/ragota"
)

func TestParseInitArgs(t *testing.T) {
	if p, err := parseInitArgs(nil); err != nil || p != "" {
		t.Errorf("no args = (%q, %v), want default path and no error", p, err)
	}
	if p, err := parseInitArgs([]string{"custom.yaml"}); err != nil || p != "custom.yaml" {
		t.Errorf("one arg = (%q, %v), want the path and no error", p, err)
	}
	if _, err := parseInitArgs([]string{"a", "b"}); err == nil {
		t.Error("two args parsed without an error")
	}
}

func TestParseSkillsArgs(t *testing.T) {
	if _, err := parseSkillsArgs(nil); err == nil {
		t.Error("missing verb parsed without an error")
	}
	if _, err := parseSkillsArgs([]string{"remove"}); err == nil {
		t.Error("unknown verb parsed without an error")
	}
	if d, err := parseSkillsArgs([]string{"install"}); err != nil || d != "" {
		t.Errorf("install = (%q, %v), want default dir and no error", d, err)
	}
	if d, err := parseSkillsArgs([]string{"install", "here"}); err != nil || d != "here" {
		t.Errorf("install here = (%q, %v), want the dir and no error", d, err)
	}
	if _, err := parseSkillsArgs([]string{"install", "a", "b"}); err == nil {
		t.Error("trailing arguments parsed without an error")
	}
}

func TestRunInitWritesTheExampleOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if code := runInit(path); code != 0 {
		t.Fatalf("first init exited %d", code)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the written config: %v", err)
	}
	if !bytes.Equal(got, assets.ConfigExample) {
		t.Error("written config differs from the embedded example")
	}

	// A second init must refuse and leave the file intact.
	if err := os.WriteFile(path, []byte("edited: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runInit(path); code == 0 {
		t.Fatal("init over an existing file exited 0")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "edited: true\n" {
		t.Error("init over an existing file changed its content")
	}
}

func TestRunSkillsInstall(t *testing.T) {
	dir := t.TempDir()

	// A file the install does not own survives it.
	foreign := filepath.Join(dir, "my-own-skill.md")
	if err := os.WriteFile(foreign, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := runSkillsInstall(dir); code != 0 {
		t.Fatalf("install exited %d", code)
	}
	for _, name := range []string{"ragota-architecture", "ragota-code-search", "ragota-index-health"} {
		p := filepath.Join(dir, name, "SKILL.md")
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s not installed: %v", p, err)
		}
		want, err := assets.Skills.ReadFile("skills/" + name + "/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the embedded skill", p)
		}
	}

	// Reinstalling refreshes a drifted copy — that is the point of installing
	// from the binary — and still leaves foreign files alone.
	drifted := filepath.Join(dir, "ragota-code-search", "SKILL.md")
	if err := os.WriteFile(drifted, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runSkillsInstall(dir); code != 0 {
		t.Fatalf("reinstall exited %d", code)
	}
	got, err := os.ReadFile(drifted)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "stale" {
		t.Error("reinstall left a drifted skill in place")
	}
	if mine, err := os.ReadFile(foreign); err != nil || string(mine) != "mine" {
		t.Errorf("install touched a file it does not own: (%q, %v)", mine, err)
	}
}
