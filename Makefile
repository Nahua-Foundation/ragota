# ragota — build & test automation.
# cgo is required (tree-sitter bindings).
export CGO_ENABLED=1

# Pre-existing unformatted file, kept as-is for now. Run `gofmt -w` on it and
# drop this exclusion when convenient.
FMT_EXCLUDE := ./internal/graph/match_test.go

# `find` has no idea that the go tool skips "_"- and "."-prefixed directories
# when expanding "./...", so every underscore-prefixed tree has to be excluded
# by hand or gofmt starts reporting other people's code — the benchmark corpus
# under $(CORPUS_DIR) is tens of thousands of files nobody here formats.
GOFILES := $(shell find . -type f -name '*.go' -not -path './testdata/*' -not -path '*/testdata/*' -not -path './.git/*' -not -path './_*/*' -not -path '$(FMT_EXCLUDE)')

PG_CONTAINER := ragota-pg-test
PG_PORT      := 55432
PG_DSN       := postgres://postgres:postgres@localhost:$(PG_PORT)/ragota_test?sslmode=disable

QDRANT_CONTAINER := ragota-qdrant-test
QDRANT_PORT      := 56333
QDRANT_URL       := http://127.0.0.1:56333

PG_E2E_CONTAINER := ragota-pg-e2e
PG_E2E_PORT      := 55434
PG_E2E_DSN       := postgres://postgres:postgres@localhost:$(PG_E2E_PORT)/ragota_e2e?sslmode=disable

# Compose v2 or newer, as either the CLI plugin or the standalone binary. v1 is
# rejected: it silently ignores deploy.resources.limits, and the C# language
# server needs its memory limit to size the .NET GC heap
# (see deploy/lsp/docker-compose.lsp.yml) — under v1 it OOM-crashed instead of
# reporting an error.
COMPOSE := $(shell \
	if docker compose version >/dev/null 2>&1; then echo "docker compose"; \
	elif docker-compose version 2>/dev/null | grep -qE 'version v?[2-9]'; then echo "docker-compose"; \
	else echo "compose-missing"; fi)
LSP_COMPOSE := RAGOTA_LSP_HOST_ROOT=$(PWD) $(COMPOSE) -f deploy/lsp/docker-compose.lsp.yml
STACK_COMPOSE := $(COMPOSE) -f deploy/docker-compose.yml
LSP_PORTS   := 7301 7302 7303 7304

BIN        := ragota
BIN_DIR    := bin
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -X main.version=$(VERSION)
INSTALL_DIR := $(shell go env GOBIN)
ifeq ($(INSTALL_DIR),)
INSTALL_DIR := $(shell go env GOPATH)/bin
endif

# Config used by `make run` / `make check-config`; override on the command
# line: make run CONFIG=deploy/config.docker.yaml
CONFIG ?= config.yaml

.DEFAULT_GOAL := build

.PHONY: up release release-snapshot \
	build binary install run check-config vet fmt fmt-check lint lint-install sqlc \
	test test-integration test-integration-all e2e test-postgres test-qdrant test-integration-postgres ci \
	lsp-build lsp-up lsp-down test-integration-lsp compose-check compose-up compose-down compose-logs help \
	corpus-clone corpus-bench corpus-measure eval eval-fast eval-validate \
	eval-compare eval-related eval-answers docs docs-serve

build:
	go build ./...

# The one version-stamped binary (--version reports it); the server, repos
# and the MCP server are its subcommands.
binary:
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BIN) ./cmd/ragota

install:
	go build -ldflags "$(LDFLAGS)" -o $(INSTALL_DIR)/$(BIN) ./cmd/ragota
	@echo "installed $(INSTALL_DIR)/$(BIN)"

# Runs the server against $(CONFIG); RAGOTA_CONFIG still works.
run: binary
	./$(BIN_DIR)/$(BIN) --config $(CONFIG)

# The whole local stack from a cold machine — qdrant, embedder, reranker,
# wiring check — then the server over a directory of repositories:
#   make up SOURCE=~/projects
up: binary
	scripts/up.sh $(SOURCE)

# Cross-build the release matrix into dist/ and stop: proves the artifacts
# without touching git or GitHub. VERSION= is required, e.g. VERSION=v0.2.0.
release-snapshot:
	scripts/release.sh --snapshot $(VERSION)

# Build the matrix, tag $(VERSION), push the tag and publish a GitHub release
# with its assets. Needs a clean tree and a one-time `gh auth login`.
release:
	scripts/release.sh $(VERSION)

# Validates the config and probes every configured dependency. Exit codes:
# 0 ok, 1 invalid config, 2 a dependency is unreachable.
check-config: binary
	./$(BIN_DIR)/$(BIN) --config $(CONFIG) --check-config

vet:
	go vet ./...

fmt:
	gofmt -w $(GOFILES)

fmt-check:
	@out="$$(gofmt -l $(GOFILES))"; \
	if [ -n "$$out" ]; then \
		echo "gofmt: the following files are not formatted:"; \
		echo "$$out"; \
		exit 1; \
	fi

# Fails when golangci-lint is missing instead of passing silently: a lint
# target that no-ops makes .golangci.yml dead config.
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint is not installed; run 'make lint-install' or see https://golangci-lint.run/usage/install/"; \
		exit 1; \
	}
	golangci-lint run

lint-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

# Regenerates the sqlc-generated query code for both SQL backends (requires sqlc).
sqlc:
	cd internal/store/postgres && sqlc generate
	cd internal/store/sqlite && sqlc generate

test:
	go test ./...

test-integration:
	go test -tags integration -run 'TestBM25AndLocalSource' ./internal/inttest/

test-integration-all:
	go test ./internal/inttest/ -v

# The whole product, from outside: builds bin-for-bin what a release ships,
# starts the server on a fixture estate, and reads the answers back through
# the HTTP API and through the mcp subcommand over stdio — the same doors users use.
e2e:
	go test -tags e2e -count=1 -timeout 10m ./e2e/ -v

# Spins up a throwaway Postgres in Docker, runs the postgres storage tests
# against it, and always tears the container down afterwards.
test-postgres:
	@docker rm -f $(PG_CONTAINER) >/dev/null 2>&1 || true
	docker run --rm -d --name $(PG_CONTAINER) \
		-e POSTGRES_USER=postgres \
		-e POSTGRES_PASSWORD=postgres \
		-e POSTGRES_DB=ragota_test \
		-p $(PG_PORT):5432 \
		postgres:16-alpine
	@echo "waiting for postgres to become ready..."
	@ok=0; \
	for i in $$(seq 1 30); do \
		if docker exec $(PG_CONTAINER) pg_isready -U postgres -d ragota_test >/dev/null 2>&1; then ok=1; break; fi; \
		sleep 1; \
	done; \
	if [ "$$ok" -ne 1 ]; then \
		echo "postgres did not become ready in time"; \
		docker stop $(PG_CONTAINER) >/dev/null 2>&1 || true; \
		exit 1; \
	fi
	@RAGOTA_TEST_POSTGRES_DSN="$(PG_DSN)" go test ./internal/store/postgres/ -v; \
	status=$$?; \
	docker stop $(PG_CONTAINER) >/dev/null 2>&1 || true; \
	exit $$status

# Spins up a throwaway Postgres in Docker and runs the full e2e suite plus the
# postgres storage tests against it (the primary-backend configuration), then
# always tears the container down.
test-integration-postgres:
	@docker rm -f $(PG_E2E_CONTAINER) >/dev/null 2>&1 || true
	docker run --rm -d --name $(PG_E2E_CONTAINER) \
		-e POSTGRES_USER=postgres \
		-e POSTGRES_PASSWORD=postgres \
		-e POSTGRES_DB=ragota_e2e \
		-p $(PG_E2E_PORT):5432 \
		postgres:16-alpine
	@echo "waiting for postgres to become ready..."
	@ok=0; \
	for i in $$(seq 1 30); do \
		if docker exec $(PG_E2E_CONTAINER) pg_isready -U postgres -d ragota_e2e >/dev/null 2>&1; then ok=1; break; fi; \
		sleep 1; \
	done; \
	if [ "$$ok" -ne 1 ]; then \
		echo "postgres did not become ready in time"; \
		docker stop $(PG_E2E_CONTAINER) >/dev/null 2>&1 || true; \
		exit 1; \
	fi
	@RAGOTA_TEST_STORAGE=postgres RAGOTA_TEST_POSTGRES_DSN="$(PG_E2E_DSN)" \
		go test ./internal/inttest/ ./internal/store/postgres/ -count=1; \
	status=$$?; \
	docker stop $(PG_E2E_CONTAINER) >/dev/null 2>&1 || true; \
	exit $$status

# Spins up a throwaway Qdrant in Docker and runs the vector-store tests against
# it. The stubbed tests agree with whatever the client sends; these are the ones
# that check Qdrant accepts it.
test-qdrant:
	@docker rm -f $(QDRANT_CONTAINER) >/dev/null 2>&1 || true
	docker run --rm -d --name $(QDRANT_CONTAINER) -p $(QDRANT_PORT):6333 qdrant/qdrant:latest
	@echo "waiting for qdrant to become ready..."
	@ok=0; \
	for i in $$(seq 1 30); do \
		if curl -sf -m 2 http://127.0.0.1:$(QDRANT_PORT)/healthz >/dev/null 2>&1; then ok=1; break; fi; \
		sleep 1; \
	done; \
	if [ "$$ok" -ne 1 ]; then \
		echo "qdrant did not become ready in time"; \
		docker stop $(QDRANT_CONTAINER) >/dev/null 2>&1 || true; \
		exit 1; \
	fi
	@RAGOTA_TEST_QDRANT_URL="$(QDRANT_URL)" go test ./internal/store/qdrant/ -count=1 -v; \
	status=$$?; \
	docker stop $(QDRANT_CONTAINER) >/dev/null 2>&1 || true; \
	exit $$status

# --- LSP refinement pass (per-language language servers in Docker) ---

# Builds the four LSP server images (go/gopls, typescript, java/jdtls, csharp).
compose-check:
	@if [ "$(COMPOSE)" = "compose-missing" ]; then \
		echo "error: Docker Compose v2+ is required (neither 'docker compose' nor a v2 'docker-compose' was found)"; \
		exit 1; \
	fi

lsp-build: compose-check
	$(LSP_COMPOSE) build

# Starts the LSP servers with the repo root mounted read-only at /workspace.
lsp-up: compose-check
	$(LSP_COMPOSE) up -d --build

lsp-down:
	$(LSP_COMPOSE) down

# Starts the LSP containers, waits until all four TCP ports accept
# connections, runs the LSP e2e tests and always tears the containers down.
# The readiness probe is Go, not nc: nc is absent from many CI images and its
# flags differ between the BSD, GNU and busybox variants.
test-integration-lsp: lsp-up
	@for p in $(LSP_PORTS); do \
		ok=0; \
		for i in $$(seq 1 60); do \
			if go run ./tools/waitport 127.0.0.1:$$p >/dev/null 2>&1; then ok=1; break; fi; \
			sleep 1; \
		done; \
		if [ "$$ok" -ne 1 ]; then \
			echo "LSP server on port $$p did not become ready in time"; \
			$(MAKE) lsp-down; \
			exit 1; \
		fi; \
		echo "port $$p ready"; \
	done
	@RAGOTA_TEST_LSP=1 RAGOTA_LSP_HOST_ROOT=$(PWD) \
		go test ./internal/inttest/ -run TestLSP -v -count=1 -timeout 20m; \
	status=$$?; \
	$(MAKE) lsp-down; \
	exit $$status

# Full local stack (app + postgres + qdrant + LSP servers).
compose-up: compose-check
	$(STACK_COMPOSE) up -d --build

compose-down:
	$(STACK_COMPOSE) down

compose-logs:
	$(STACK_COMPOSE) logs -f app

# --- benchmark corpus & retrieval evaluation (tools/corpus, tools/eval) ---
#
# The corpus targets answer "how much did the extractor find?"; the eval
# targets answer "did an answer get better?". Both are Python 3 standard
# library only and both work against a throwaway server they start themselves
# (eval) or a running one (corpus).

# Where the corpus checkouts live. ~15 GB with every repository cloned.
#
# The leading underscore is load-bearing: the corpus is other people's source,
# including Go packages with their own imports, and `go build ./...` walks
# every directory in the tree. Cloned into a plain "corpus" it breaks the build
# — `no required module provides package github.com/instana/go-sensor` from
# robotshop — and with it `make ci`, for anyone who ran `make corpus-clone` as
# documented. The go tool skips directories whose name starts with "_" or "."
# when expanding "...", so this name keeps the corpus invisible to the build.
CORPUS_DIR ?= _corpus
# Throwaway databases, BM25 indexes and result files for eval runs.
EVAL_WORK  ?= /tmp/ragota-eval
# Extra flags handed to the eval scripts, e.g.
#   make eval EVAL_ARGS="--repo consul --shape route"
EVAL_ARGS  ?=

corpus-clone:
	tools/corpus/clone.sh -d $(CORPUS_DIR)

corpus-bench:
	tools/corpus/bench.py --corpus $(CORPUS_DIR) $(CORPUS_ARGS)

corpus-measure:
	tools/corpus/measure.py --corpus $(CORPUS_DIR) $(CORPUS_ARGS)

# Checks tools/eval/queries.jsonl against the corpus *sources*: every expected
# file exists and every anchor is still on the recorded line. No server, no
# index — the ground truth must never be validated by the system under test.
eval-validate:
	tools/eval/run.py --validate --corpus $(CORPUS_DIR) $(EVAL_ARGS)

# Builds cmd/ragota, indexes the repositories the query set needs into
# $(EVAL_WORK), asks every question through /search and /context and scores the
# answers with recall@k, MRR and nDCG@10.
eval: eval-validate
	tools/eval/run.py --corpus $(CORPUS_DIR) --work $(EVAL_WORK) $(EVAL_ARGS)

# A/B: the same query set under two binaries or two configurations, with the
# per-query delta. Examples:
#   make eval-compare EVAL_ARGS="--a-mode keyword --b-mode hybrid --no-reindex"
#   make eval-compare EVAL_ARGS="--b-variant rerank --rerank-url http://localhost:8090"
#   make eval-compare EVAL_ARGS="--a-binary /tmp/before --b-binary /tmp/after"
eval-compare:
	tools/eval/compare.py --corpus $(CORPUS_DIR) --work $(EVAL_WORK) $(EVAL_ARGS)

# Scores the graph expansion /context returns around each hit, which `eval`
# reads past: does the item that answers the question also name the second file
# the answer needs, and does the expansion ever supply an answer ranking missed.
# The six repositories that index in minutes rather than an hour, for a run
# that has to fit on a laptop: it excludes elasticsearch, grafana and medusa,
# which are four fifths of the corpus by size, and the cross-repository
# questions that reach into elasticsearch.
eval-fast:
	tools/eval/run.py --corpus $(CORPUS_DIR) --work $(EVAL_WORK) --scope in-repo \
	  --repo petclinic --repo boutique --repo robotshop --repo eshop \
	  --repo conductor --repo jellyfin $(EVAL_ARGS)

eval-related:
	tools/eval/related.py --corpus $(CORPUS_DIR) --work $(EVAL_WORK) $(EVAL_ARGS)

# Puts the /context package in front of a local model and grades the answer
# against the recorded file, line and anchor, with the retrieval ceiling
# reported apart from the score. Needs an ollama-compatible endpoint:
#   make eval-answers EVAL_ARGS="--model qwen2.5:1.5b --judge --control"
eval-answers:
	tools/eval/answer.py --corpus $(CORPUS_DIR) --work $(EVAL_WORK) $(EVAL_ARGS)

# The documentation site (docs/, Docusaurus). `docs` builds the static site
# into docs/build; `docs-serve` runs the live-reloading dev server.
docs:
	cd docs && npm install --no-audit --no-fund && npm run build

docs-serve:
	cd docs && npm install --no-audit --no-fund && npm run start

# Everything CI runs, including lint and the integration suite.
# test-postgres is in ci deliberately: postgres is the primary relational
# backend, and without it the conformance suite only ever runs against sqlite.
ci: build vet fmt-check lint test test-integration test-postgres

help:
	@echo "build          compile every package"
	@echo "binary         build $(BIN_DIR)/$(BIN) (version $(VERSION))"
	@echo "install        build $(BIN) into $(INSTALL_DIR)"
	@echo "run            run the server with --config $(CONFIG)"
	@echo "check-config   validate the config and probe its dependencies"
	@echo "test           unit tests            test-integration  tagged integration tests"
	@echo "test-integration-all  full in-process integration suite    test-integration-lsp  with LSP containers"
	@echo "e2e            the shipped binary driven from outside (HTTP + MCP over stdio)"
	@echo "docs           build the documentation site (docs/build)   docs-serve  live dev server"
	@echo "test-postgres  storage tests on a throwaway postgres"
	@echo "lint           golangci-lint (lint-install to get it)"
	@echo "compose-up     start the deploy/docker-compose.yml stack"
	@echo "lsp-up         start only the LSP servers (deploy/lsp)"
	@echo "ci             build vet fmt-check lint test test-integration test-postgres"
	@echo "corpus-clone   clone the benchmark corpus into $(CORPUS_DIR)"
	@echo "corpus-bench   extraction counts per repository (needs a running server)"
	@echo "eval           retrieval quality: recall@k, MRR, nDCG@10 over tools/eval/queries.jsonl"
	@echo "eval-validate  re-check the eval ground truth against the corpus sources"
	@echo "eval-compare   the same query set under two binaries or two configurations"
	@echo "eval-related   score the /context graph expansion, not just the hits"
	@echo "eval-answers   grade what a local model answers from a /context package"
