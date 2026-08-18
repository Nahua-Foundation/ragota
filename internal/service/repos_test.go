package service

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// What a user types at a shell is an id, a name or a path — including a
// relative one, so that a "." typed inside a project means it.
func TestResolveRepoRef(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	list := []*repos.Repo{
		{ID: "alpha-000000000001", Name: "alpha", Path: "/home/dev/projects/alpha"},
		{ID: "beta-000000000002", Name: "beta", Path: "/home/dev/projects/beta"},
		{ID: "beta-000000000003", Name: "beta", Path: "/srv/other/beta"},
		{ID: "here-000000000004", Name: "here", Path: cwd},
	}

	tests := []struct {
		name, ref, want string
		wantErr         bool
	}{
		{name: "by id", ref: "alpha-000000000001", want: "alpha-000000000001"},
		{name: "by name", ref: "alpha", want: "alpha-000000000001"},
		{name: "by path", ref: "/home/dev/projects/alpha", want: "alpha-000000000001"},
		{name: "by relative path", ref: ".", want: "here-000000000004"},
		{name: "an ambiguous name is refused", ref: "beta", wantErr: true},
		{name: "...but its id is not", ref: "beta-000000000003", want: "beta-000000000003"},
		{name: "a prefix is not a match", ref: "alph", wantErr: true},
		{name: "an unknown repository", ref: "gamma", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveRepoRef(list, tt.ref)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveRepoRef(%q) error = %v, wantErr %v", tt.ref, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.ID != tt.want {
				t.Errorf("resolveRepoRef(%q) = %q, want %q", tt.ref, got.ID, tt.want)
			}
		})
	}

	// The ambiguity is reported with the ids that resolve it; an error telling
	// the user only that their name is ambiguous leaves them with nothing to
	// type next.
	if _, err := resolveRepoRef(list, "beta"); err == nil ||
		!strings.Contains(err.Error(), "beta-000000000002") ||
		!strings.Contains(err.Error(), "beta-000000000003") {
		t.Errorf("ambiguous name error = %v, want both ids in it", err)
	}

	// An unknown reference is storage.ErrNotFound, so callers can decorate it
	// (the CLI appends how to see what is registered) without string-matching.
	if _, err := resolveRepoRef(list, "gamma"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("unknown repo error = %v, want errors.Is(..., storage.ErrNotFound)", err)
	}
}
