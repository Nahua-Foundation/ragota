# Changelog

Release notes come from this file: the release machinery lifts the section
matching the tag (`## vX.Y.Z`) into the GitHub release, and falls back to
the raw git log when a version has no section here. Newest first.

## v0.2.0 — 2026-08-19

The v2 release: **ragota-core and its MCP server merge into one module,
one name, one binary** — `ragota`, with the server, `repos` administration
and the read-only MCP server as subcommands of one file. The v1 local tool
this repository used to hold is the history before the merge.

### Added

- `ragota init` and `ragota skills install` — the binary writes its own
  example config and agent skills, so a release archive is just the binary.
- Agent skills (`skills/`): the judgement layer over the ten MCP tools —
  when the index beats the agent's own glob/grep/read, and the
  empty-answer protocol.
- An end-to-end suite driving the shipped binary through both doors, HTTP
  and MCP over stdio: `make e2e`, ~6 s, no Docker, no network.
- Query-side instruction for instruction-aware embedders
  (Qwen3-Embedding).
- CI: build, vet, fmt-check and lint on every push. A release is a pushed
  tag — the workflow builds darwin arm64/amd64 and musl-static linux
  amd64/arm64 and publishes them; this is the first release to ship linux
  binaries at all.
- The documentation site, deployed to GitHub Pages on every push that
  touches it.
- Apache-2.0, ahead of going public.

### Changed

- The data directory is `~/.ragota` — the `-core` suffix retires
  everywhere.
- `internal/` regrouped by role — app, server, store, index, domain — and
  the public Go client moved to `client/`.
- The vendored zapx retires: upstream v17.1.9 ships the chunk-table fix
  this tree carried as a local patch.

### Fixed

- The linux cross-build never survived its own shell quoting — latent
  since the release script was written, caught by the release workflow's
  dry run.
- The built-in dimension for `qwen3-embedding:0.6b` was half again its
  real size, creating a vector collection nothing could ever insert into.
- One oversized snippet cost the whole query its reranking.
- Compacting an already-compacted index stalled its caller for good.
- `--check-config` passed a vector embedder the server then refused to
  construct.
- A fatal start-up error under `--interactive` exited with nothing
  printed.

**Full log**: https://github.com/Nahua-Foundation/ragota/compare/v0.1.0...v0.2.0
