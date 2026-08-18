#!/usr/bin/env bash
# Build the release matrix, tag, and publish a GitHub release — from this
# machine, with no CI in the loop. `--snapshot` stops after the builds so the
# matrix can be proven without touching git or GitHub.
#
#   make release-snapshot VERSION=v0.2.0     # builds into dist/, publishes nothing
#   make release VERSION=v0.2.0              # tag + gh release create with assets
#
# cgo is why this script exists instead of a cross-compile one-liner: the
# tree-sitter grammars are C. Both darwin targets build natively on a Mac
# (Apple clang is a multi-arch compiler, and cgo passes the right -arch for
# GOARCH). Linux needs a cross C toolchain — zig serves as one — and is built
# only when zig is installed, skipped loudly when it is not: a release with
# fewer platforms is honest, a release that silently pretends is not.
set -euo pipefail

MODE=publish
if [ "${1:-}" = "--snapshot" ]; then
    MODE=snapshot
    shift
fi
VERSION="${1:?usage: release.sh [--snapshot] vX.Y.Z}"
case "$VERSION" in
v[0-9]*) ;;
*)
    echo "release.sh: version must look like v0.2.0, got '$VERSION'" >&2
    echo "  (make release VERSION=v0.2.0 — the VERSION= is required)" >&2
    exit 1
    ;;
esac

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
DIST="dist/$VERSION"

if [ "$MODE" = publish ]; then
    command -v gh >/dev/null || { echo "release.sh: gh is required to publish (brew install gh && gh auth login)" >&2; exit 1; }
    gh auth token >/dev/null 2>&1 || { echo "release.sh: gh is not authenticated; run: gh auth login" >&2; exit 1; }
    [ -z "$(git status --porcelain)" ] || { echo "release.sh: the tree is dirty; a release builds exactly what a tag names" >&2; exit 1; }
    if git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null; then
        echo "release.sh: tag $VERSION already exists" >&2
        exit 1
    fi
fi

rm -rf "$DIST"
mkdir -p "$DIST"

build() {
    local goos="$1" goarch="$2" cc="${3:-}"
    local name="ragota_${VERSION}_${goos}_${goarch}"
    local out="$DIST/$name"
    echo "  $goos/$goarch"
    mkdir -p "$out"
    CGO_ENABLED=1 GOOS="$goos" GOARCH="$goarch" ${cc:+CC="$cc"} \
        go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out/ragota" ./cmd/server
    cp README.md "$out/"
    tar -czf "$DIST/$name.tar.gz" -C "$DIST" "$name"
    rm -r "$out"
}

echo "building $VERSION into $DIST"
build darwin arm64
build darwin amd64
if command -v zig >/dev/null; then
    build linux amd64 "zig cc -target x86_64-linux-musl"
    build linux arm64 "zig cc -target aarch64-linux-musl"
else
    echo "  linux targets skipped: no cross C compiler (brew install zig enables them)"
fi

(cd "$DIST" && shasum -a 256 ./*.tar.gz > checksums.txt)
echo "artifacts:"
ls -lh "$DIST" | awk 'NR>1 {print "  " $5 "\t" $9}'

if [ "$MODE" = snapshot ]; then
    echo "snapshot only: nothing tagged, nothing published"
    exit 0
fi

# The tag is annotated and pushed first: the release must point at history
# that exists on the remote, and `--verify-tag` below makes gh check exactly
# that instead of quietly creating one of its own.
git tag -a "$VERSION" -m "ragota $VERSION"
git push -q origin "$VERSION"

PREV="$(git describe --tags --abbrev=0 "$VERSION^" 2>/dev/null || true)"
NOTES="$(git log --oneline ${PREV:+$PREV..}"$VERSION" | sed 's/^/- /')"
gh release create "$VERSION" "$DIST"/*.tar.gz "$DIST/checksums.txt" \
    --verify-tag --title "ragota $VERSION" --notes "$NOTES"
echo "published: $(gh release view "$VERSION" --json url -q .url)"
