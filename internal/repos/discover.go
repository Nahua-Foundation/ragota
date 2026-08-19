package repos

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// DefaultDiscoveryDepth bounds how far below the source root Discover looks
// for repositories.
//
// Three levels covers the layouts people actually keep projects in —
// ~/projects/repo, ~/src/org/repo and ~/code/github.com/org/repo — without
// turning "point it at my home directory" into a full traversal of it. The
// scan is cheaper than the bound suggests: it never descends into a directory
// it has already recognized as a repository, so the depth is only spent on the
// intermediate directories that organize them.
const DefaultDiscoveryDepth = 3

// Discover returns the repositories at or below root, as absolute paths sorted
// for a stable registration order.
//
// A repository is a directory containing .git. That is the marker git itself
// uses, it is the one thing every checkout has, and it is deliberately not the
// build-manifest test that svcdetect applies inside a repository: a go.mod or
// a package.json marks a *service*, and a monorepo full of them is one
// repository, not forty.
//
// The rules, in order:
//
//   - root itself is a repository: it is the only result. Its submodules and
//     nested checkouts belong to it, so the scan does not descend past it. This
//     is what makes `--source ./my-project` work without a wrapper directory.
//   - otherwise every directory with a .git below root, down to maxDepth, and
//     the scan stops at each one it finds.
//   - otherwise root itself, on the assumption that a directory someone
//     pointed the indexer at holds code even when it is not version-controlled.
//     The caller is told which of these three happened, because the last one is
//     a guess and the first two are not.
//
// Directories whose name begins with a dot are skipped: they are configuration
// and caches, and descending into .git alone would cost more than the rest of
// the scan.
func Discover(root string, maxDepth int) ([]string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve source path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("source path: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source path is not a directory: %s", abs)
	}
	if maxDepth < 0 {
		maxDepth = 0
	}

	if IsRepoRoot(abs) {
		return []string{abs}, nil
	}

	var found []string
	scan(abs, 1, maxDepth, &found)
	if len(found) == 0 {
		return []string{abs}, nil
	}
	sort.Strings(found)
	return found, nil
}

// scan walks one directory level, recording repository roots and recursing
// into the directories that are not repositories themselves.
//
// Read errors are swallowed rather than aborting: a home directory reliably
// contains something the process may not read, and one unreadable directory
// must not cost the user every repository beside it.
func scan(dir string, depth, maxDepth int, found *[]string) {
	if depth > maxDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue
		}
		child := filepath.Join(dir, e.Name())
		if IsRepoRoot(child) {
			*found = append(*found, child)
			continue
		}
		scan(child, depth+1, maxDepth, found)
	}
}

// IsRepoRoot reports whether dir is the root of a git checkout.
//
// .git is accepted as either a directory or a file: a worktree or a submodule
// checkout has it as a file holding a gitdir pointer, and WalkFiles already
// handles both forms for the same reason.
func IsRepoRoot(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}
