package repos

import (
	"os"
	"path/filepath"

	"github.com/Nahua-Foundation/ragota/internal/config"
)

// WalkFiles returns the indexable files under root: everything the ignore
// patterns keep and DetectLanguage recognizes, with paths relative to root.
//
// "The ignore patterns" is the configured list plus, unless repos.use_gitignore
// is off, whatever the checkout excludes itself — see config.IgnorePatterns for
// how the two compose.
//
// Both sources walk their checkout the same way, so it is one function rather
// than one per source: the two copies this replaces had already drifted apart
// while sharing the same dead pruning branch, and a walk that is wrong in one
// source and right in the other makes retrieval depend on how a repository was
// added.
func WalkFiles(root string, ignorePatterns []string) ([]*RepoFile, error) {
	// Built once for the whole walk. IgnorePatterns caches its verdict per
	// repository and path; constructing it inside the callback, as both walkers
	// used to, threw that cache away before a single lookup could hit it.
	ignore := config.NewIgnorePatterns(ignorePatterns)

	var files []*RepoFile
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			// Prune rather than filter: an excluded node_modules is thousands
			// of entries that would otherwise be read, stat'ed and matched
			// against every pattern one file at a time.
			if SkipDir(root, path, d.Name(), ignore) {
				return filepath.SkipDir
			}
			return nil
		}
		// A worktree or submodule checkout has .git as a file.
		if d.Name() == ".git" {
			return nil
		}

		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if ignore.ShouldIgnore(root, path) {
			return nil
		}
		lang := DetectLanguage(relPath)
		if lang == "" {
			return nil
		}

		// WalkDir hands out directory entries without stat'ing them, so the
		// size is fetched only for the files that survived the filters above.
		info, err := d.Info()
		if err != nil {
			return err
		}

		files = append(files, &RepoFile{
			Path:     relPath,
			Hash:     "", // Computed by the caller once the content is read
			Language: lang,
			Size:     info.Size(),
		})
		return nil
	})

	return files, err
}

// SkipDir reports whether a traversal of root must not descend into the
// directory at path, whose base name is name.
//
// It is exported because WalkFiles is not the only traversal of a repository:
// the filesystem watcher installs one watch per directory and has to leave out
// exactly the directories this walk leaves out. If the two disagreed, the
// watcher would either index what the walker excludes — a change under
// node_modules re-appearing in the index until the next full pass removed it —
// or spend a file descriptor per directory of it, which is how a watcher runs
// out of them. Sharing the predicate makes that agreement a fact rather than a
// pair of comments hoping to stay in sync.
//
// .git is matched as a whole path component. The substring test this replaces
// ran over the absolute path, so it also caught .github and .gitlab-ci.yml —
// and every file of a repository that merely sat under a directory such as
// /srv/.gitmirrors, which indexed as empty.
//
// The directory form of the question is asked explicitly, because a .gitignore
// distinguishes: "build/" excludes a directory and never a file of that name.
func SkipDir(root, path, name string, ignore *config.IgnorePatterns) bool {
	if name == ".git" {
		return true
	}
	// The root is exempt: patterns describe what to leave out of a repository,
	// so one that happens to match the root must not silently index nothing.
	return path != root && ignore.ShouldIgnoreDir(root, path)
}
