package config

import (
	"log/slog"
	"os"
)

// MigrateLegacyHome moves the data directory earlier releases kept under
// ~/.ragota-core to its current home, ~/.ragota. It runs only when the config
// actually uses the built-in default paths: a config that names its own
// locations — including ones still pointing into ~/.ragota-core on purpose —
// is left entirely alone, and so is a machine where both directories already
// exist, because guessing which of the two the user means is worse than
// asking.
func MigrateLegacyHome(cfg *Config) {
	usingDefaults := cfg.Storage.SQLite != nil && cfg.Storage.SQLite.Path == DefaultSQLitePath ||
		cfg.Indexes.BM25 != nil && cfg.Indexes.BM25.Path == DefaultBM25Path
	if !usingDefaults {
		return
	}
	oldBase, err := ExpandPathErr("~/.ragota-core")
	if err != nil {
		return
	}
	newBase, err := ExpandPathErr("~/.ragota")
	if err != nil {
		return
	}
	if _, err := os.Stat(oldBase); err != nil {
		return // nothing to migrate
	}
	if _, err := os.Stat(newBase); err == nil {
		return // both exist; not ours to pick
	}
	if err := os.Rename(oldBase, newBase); err != nil {
		slog.Warn("legacy data found in ~/.ragota-core but moving it failed; move it to ~/.ragota by hand",
			"err", err)
		return
	}
	slog.Info("moved the data directory of earlier releases", "from", oldBase, "to", newBase)
}
