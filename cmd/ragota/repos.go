package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/repos"
	"github.com/Nahua-Foundation/ragota/internal/service"
	"github.com/Nahua-Foundation/ragota/internal/setup"
)

// The `repos` subcommand: what the index is made of, and which part of it the
// runs are about.
//
// It exists because the working set became something a user has to be able to
// see. A repository stays registered forever and only --source decides
// membership, so without this the answer to "why is my search not finding
// anything from project X" lives in a database column reachable through curl.
//
// The actions are deliberately few. Anything that changes what is *indexed* —
// adding, deleting, reindexing — stays in the API, where it has been all
// along; these three only move the boundary of what a question is answered
// from, which is the one thing --source alone cannot express.
const (
	reposList       = "list"
	reposActivate   = "activate"
	reposDeactivate = "deactivate"
)

// reposArgs is a parsed `repos` invocation.
type reposArgs struct {
	action string // one of the three above
	ref    string // the repository activate/deactivate names; empty for list
}

// parseReposArgs validates the words after `repos`. Like the subcommand itself,
// an unrecognized word is an error rather than something to ignore: `repos
// activte foo` must not silently print a list.
func parseReposArgs(args []string) (reposArgs, error) {
	if len(args) == 0 {
		return reposArgs{}, fmt.Errorf("%s needs one of: list, activate REPO, deactivate REPO", commandRepos)
	}
	switch args[0] {
	case reposList:
		if len(args) > 1 {
			return reposArgs{}, fmt.Errorf("unexpected arguments after %q: %s", args[0], strings.Join(args[1:], " "))
		}
		return reposArgs{action: reposList}, nil
	case reposActivate, reposDeactivate:
		switch len(args) {
		case 1:
			return reposArgs{}, fmt.Errorf("%s %s needs a repository: its id, its name or its path", commandRepos, args[0])
		case 2:
			return reposArgs{action: args[0], ref: args[1]}, nil
		default:
			return reposArgs{}, fmt.Errorf("unexpected arguments after %q: %s", args[1], strings.Join(args[2:], " "))
		}
	default:
		return reposArgs{}, fmt.Errorf("unknown %s subcommand %q", commandRepos, args[0])
	}
}

// runRepos answers one `repos` invocation and returns the process exit code.
//
// It opens the store, since the composition of the index is written down
// nowhere else, and closes it before returning — which is why the caller's
// os.Exit is outside this function and not in it.
func runRepos(cfg *config.Config, args reposArgs) int {
	ctx := context.Background()
	svc, err := setup.Build(ctx, cfg)
	if err != nil {
		return fail("cannot open the index: %v", err)
	}
	defer func() { _ = svc.Close(ctx) }()

	switch args.action {
	case reposList:
		return listRepos(ctx, svc)
	case reposActivate:
		return setRepoActive(ctx, svc, args.ref, true)
	case reposDeactivate:
		return setRepoActive(ctx, svc, args.ref, false)
	default:
		// parseReposArgs admits nothing else. This is here so that adding an
		// action there and forgetting it here is a message rather than a
		// silent listing.
		return fail("unknown %s subcommand %q", commandRepos, args.action)
	}
}

// listRepos prints every registered repository with the state that decides
// whether an unscoped search reaches it.
//
// Every one of them, dormant included: this is the command that says what the
// dashboard is leaving out, so filtering it would leave nowhere at all to see
// a repository that has gone quiet.
func listRepos(ctx context.Context, svc *service.Service) int {
	all, err := svc.ListRepos(ctx)
	if err != nil {
		return fail("cannot list the repositories: %v", err)
	}
	if len(all) == 0 {
		fmt.Println("no repositories registered")
		fmt.Println("point a run at a directory to register some: ragota --source DIR run")
		return 0
	}

	nameW, idW := len("REPO"), len("ID")
	for _, r := range all {
		nameW = max(nameW, len(r.Name))
		idW = max(idW, len(r.ID))
	}
	active := 0
	fmt.Printf("%-7s  %-*s  %-*s  %s\n", "STATE", nameW, "REPO", idW, "ID", "PATH")
	for _, r := range all {
		state := "dormant"
		if r.Active {
			state, active = "active", active+1
		}
		fmt.Printf("%-7s  %-*s  %-*s  %s\n", state, nameW, r.Name, idW, r.ID, r.Path)
	}

	noun := "repositories"
	if len(all) == 1 {
		noun = "repository"
	}
	fmt.Printf("\n%d %s, %d active\n", len(all), noun, active)
	if active == 0 {
		fmt.Println("nothing is active: /search and /context answer nothing until a request names a repository")
	}
	return 0
}

// setRepoActive moves one repository into or out of the working set and leaves
// the rest of it alone.
//
// SetActiveRepos replaces the set rather than editing it — that is the
// operation --source needs — so this reads the set, changes one row and writes
// the lot back. Two of these racing each other, or one racing a --source run,
// leaves the last writer's set in place; they are keystrokes at a shell, and
// ordering them properly would cost a transaction spanning two processes to buy
// nothing.
func setRepoActive(ctx context.Context, svc *service.Service, ref string, active bool) int {
	all, err := svc.ListRepos(ctx)
	if err != nil {
		return fail("cannot list the repositories: %v", err)
	}
	repo, err := findRepo(all, ref)
	if err != nil {
		return fail("%v", err)
	}
	if repo.Active == active {
		fmt.Printf("%s is already %s\n", repo.Name, activeWord(active))
		return 0
	}

	ids := make([]string, 0, len(all))
	for _, r := range all {
		on := r.Active
		if r.ID == repo.ID {
			on = active
		}
		if on {
			ids = append(ids, r.ID)
		}
	}
	if err := svc.SetActiveRepos(ctx, ids); err != nil {
		return fail("cannot change the working set: %v", err)
	}

	fmt.Printf("%s (%s) is now %s; %d of %d registered repositories active\n",
		repo.Name, repo.ID, activeWord(active), len(ids), len(all))
	if !active {
		// Said once, here, because the word "dormant" suggests neither: the
		// index is untouched and the next --source or activate brings it back.
		fmt.Println("its index, edges and coverage are kept; naming it again restores it")
	}
	if len(ids) == 0 {
		fmt.Println("the working set is now empty: /search and /context answer nothing until a request names a repository")
	}
	return 0
}

// findRepo resolves what a user typed at a shell to one repository: its id, its
// name, or its path. A path is resolved against the working directory first, so
// that `repos deactivate .` inside a project means that project.
//
// Matching is exact in all three. A repository whose name is a prefix of
// another's is a normal thing to have — "gateway" and "gateway-v2" — and
// guessing between them here would deactivate the wrong project silently.
func findRepo(all []*repos.Repo, ref string) (*repos.Repo, error) {
	for _, r := range all {
		if r.ID == ref {
			return r, nil
		}
	}

	var named []*repos.Repo
	for _, r := range all {
		if r.Name != "" && r.Name == ref {
			named = append(named, r)
		}
	}
	if len(named) == 1 {
		return named[0], nil
	}
	if len(named) > 1 {
		// Two checkouts of the same project under different roots, which the
		// index keeps apart by id because their paths differ.
		ids := make([]string, 0, len(named))
		for _, r := range named {
			ids = append(ids, r.ID)
		}
		return nil, fmt.Errorf("%d repositories are named %q; name one by its id: %s",
			len(named), ref, strings.Join(ids, " "))
	}

	if abs, err := filepath.Abs(config.ExpandPath(ref)); err == nil {
		for _, r := range all {
			if r.Path == abs {
				return r, nil
			}
		}
	}
	return nil, fmt.Errorf("no repository matches %q; `ragota %s %s` shows what is registered",
		ref, commandRepos, reposList)
}

func activeWord(active bool) string {
	if active {
		return "active"
	}
	return "dormant"
}

// fail reports why a subcommand could not do its job and gives the exit code to
// leave with. It writes to stderr so that a failed `repos list` cannot be
// mistaken for an empty one by whatever is reading the pipe.
func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "ragota: "+format+"\n", args...)
	return exitFailure
}
