// Package ragota carries the files the binary re-emits, so that a release
// archive is just the binary: the annotated example config (written by
// `ragota init`) and the agent skills (written by `ragota skills install`).
// Both are embedded from the repository sources at build time — what the
// binary writes is exactly what the tree it was built from held, and the
// skills can never be from a different version than the tool descriptions
// they quote numbers from.
package ragota

import "embed"

// ConfigExample is config.example.yaml, verbatim: every key documented with
// its reasoning.
//
//go:embed config.example.yaml
var ConfigExample []byte

// Skills holds the agent skills, one directory per skill under "skills/".
// skills/README.md is about this repository, not part of a skill, and is
// deliberately not embedded.
//
//go:embed skills/ragota-architecture skills/ragota-code-search skills/ragota-index-health
var Skills embed.FS
