# ragota-core service image.
# cgo is mandatory (the tree-sitter bindings are C), so this is a glibc build
# on debian rather than a static alpine one.
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first: they change far less often than the sources.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ENV CGO_ENABLED=1
RUN go build -ldflags "-X main.version=${VERSION}" -o /out/ragota-core ./cmd/server

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/*

# git is needed by the git repo source (clone/fetch run as subprocesses);
# curl only by the compose healthcheck.

RUN useradd --create-home --uid 10001 ragota

# Docker seeds a fresh named volume from the image path, ownership included,
# so these two must exist and belong to the app user before any volume is
# mounted over them — otherwise the non-root process cannot write.
RUN mkdir -p /workspace /var/lib/ragota && chown -R ragota:ragota /workspace /var/lib/ragota

USER ragota

COPY --from=build /out/ragota-core /usr/local/bin/ragota-core

ENV RAGOTA_CONFIG=/etc/ragota/config.yaml
EXPOSE 8080

# --check-config exits non-zero when the config is invalid (1) or a
# dependency is unreachable (2); compose healthchecks can use /health instead.
ENTRYPOINT ["/usr/local/bin/ragota-core"]
