package bootstrap

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/config"
)

// TestBM25IndexPathsDefaultsOn pins the default that the full 103-question set
// bought: a config that says nothing about index_paths gets the field, and only
// an explicit false declines it. A config written before the setting existed is
// the common case, and it should not be the one left without the measurement.
func TestBM25IndexPathsDefaultsOn(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		cfg  *config.BM25IndexConfig
		want bool
	}{
		{"no bm25 block at all", nil, true},
		{"bm25 block without the key", &config.BM25IndexConfig{Enabled: true}, true},
		{"explicit true", &config.BM25IndexConfig{Enabled: true, IndexPaths: &on}, true},
		{"explicit false", &config.BM25IndexConfig{Enabled: true, IndexPaths: &off}, false},
	}
	for _, tc := range cases {
		if got := bm25IndexPaths(tc.cfg); got != tc.want {
			t.Errorf("%s: index_paths = %v, want %v", tc.name, got, tc.want)
		}
	}
}
