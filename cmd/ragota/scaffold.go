package main

// The scaffolding subcommands make the release archive be just the binary:
// `ragota init` writes the annotated example config and `ragota skills
// install` writes the agent skills, both embedded at build time from the
// repository sources (see embed.go at the module root). Neither loads the
// config or touches a database — they write files and exit.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	assets "github.com/Nahua-Foundation/ragota"
)

const (
	// skillsVerbInstall is the only skills verb today; the verb is required
	// so a future `skills list` does not change what `ragota skills` means.
	skillsVerbInstall = "install"

	// defaultSkillsDir is where `skills install` writes without an argument:
	// the project-level skills directory of the workspace an agent analyzes
	// code in — which is usually not this repository.
	defaultSkillsDir = ".claude/skills"
)

// parseInitArgs validates the words after `init`: at most one, the target
// path. An empty result means "the path --config or RAGOTA_CONFIG would
// read", resolved by the caller, because the file init writes is the file
// those would load.
func parseInitArgs(args []string) (string, error) {
	switch len(args) {
	case 0:
		return "", nil
	case 1:
		return args[0], nil
	default:
		return "", fmt.Errorf("%s takes at most one path, got: %s", commandInit, strings.Join(args, " "))
	}
}

// parseSkillsArgs validates the words after `skills`: the install verb,
// optionally followed by a target directory. An empty result means
// defaultSkillsDir.
func parseSkillsArgs(args []string) (string, error) {
	if len(args) == 0 || args[0] != skillsVerbInstall {
		return "", fmt.Errorf("%s needs a verb: %s [dir]", commandSkills, skillsVerbInstall)
	}
	switch len(args) {
	case 1:
		return "", nil
	case 2:
		return args[1], nil
	default:
		return "", fmt.Errorf("unexpected arguments after %q: %s", args[1], strings.Join(args[2:], " "))
	}
}

// runInit writes the example config to path and reports the exit code. An
// existing file is never overwritten: the config is where a deployment keeps
// its decisions, and "init" must not be the word that discards them.
func runInit(path string) int {
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "ragota %s: %s already exists and will not be overwritten; move it away or name another path\n", commandInit, path)
		return exitFailure
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "ragota %s: %v\n", commandInit, err)
		return exitFailure
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "ragota %s: %v\n", commandInit, err)
			return exitFailure
		}
	}
	if err := os.WriteFile(path, assets.ConfigExample, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "ragota %s: %v\n", commandInit, err)
		return exitFailure
	}
	fmt.Printf("wrote %s — every key documented, everything optional commented out\n", path)
	fmt.Printf("next: edit it, then `ragota --config %s --check-config` probes everything it names\n", path)
	return 0
}

// runSkillsInstall writes the embedded skills under dir, one directory per
// skill. Files the install owns are overwritten — the point of installing
// from the binary is that skill text and tool descriptions share a version —
// and files it does not know are left alone.
func runSkillsInstall(dir string) int {
	names := map[string]bool{}
	err := fs.WalkDir(assets.Skills, "skills", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, "skills/")
		names[rel[:strings.IndexByte(rel, '/')]] = true
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		data, err := assets.Skills.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ragota %s %s: %v\n", commandSkills, skillsVerbInstall, err)
		return exitFailure
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	fmt.Printf("installed %s into %s\n", strings.Join(sorted, ", "), dir)
	fmt.Println("the skills are versioned with the binary: run this again after upgrading ragota")
	return 0
}
