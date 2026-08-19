#!/usr/bin/env bash
# The release machinery, in one place, three doors into it:
#
#   make release VERSION=v0.2.0            # tag + push; CI builds and publishes
#   make release-snapshot VERSION=v0.2.0   # build the matrix into dist/, publish nothing
#   release.sh --from-tag v0.2.0           # what CI runs on the pushed tag
#
# A release is a pushed tag: the default mode checks the tree, tags, pushes,
# and stops — .github/workflows/release.yml picks the tag up, runs the
# --from-tag mode on a macOS runner and publishes the GitHub release. The
# same script builds in every mode, so `release-snapshot` proves locally the
# exact artifacts CI will publish.
#
# cgo is why this script exists instead of a cross-compile one-liner: the
# tree-sitter grammars are C. Both darwin targets build natively on a Mac
# (Apple clang is a multi-arch compiler, and cgo passes the right -arch for
# GOARCH). Linux needs a cross C toolchain — zig serves as one. A snapshot
# without zig skips the linux targets loudly; a --from-tag build refuses to
# run without them, because a published release with silently missing
# platforms is worse than a failed one.
set -euo pipefail

MODE=tag
case "${1:-}" in
--snapshot)
    MODE=snapshot
    shift
    ;;
--from-tag)
    MODE=from-tag
    shift
    ;;
esac
VERSION="${1:?usage: release.sh [--snapshot|--from-tag] vX.Y.Z}"
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

if [ "$MODE" = tag ]; then
    [ -z "$(git status --porcelain)" ] || { echo "release.sh: the tree is dirty; a release builds exactly what a tag names" >&2; exit 1; }
    if git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null; then
        echo "release.sh: tag $VERSION already exists" >&2
        exit 1
    fi
    # Annotated and pushed; the release workflow does the rest.
    git tag -a "$VERSION" -m "ragota $VERSION"
    git push -q origin "$VERSION"
    echo "tagged $VERSION and pushed; the release workflow builds and publishes it:"
    echo "  https://github.com/Nahua-Foundation/ragota/actions/workflows/release.yml"
    exit 0
fi

if [ "$MODE" = from-tag ]; then
    # The workflow checked the tag out; hold it to its word — the artifacts
    # must be built from exactly the commit the tag names.
    if [ "$(git rev-parse "refs/tags/$VERSION^{commit}" 2>/dev/null)" != "$(git rev-parse HEAD)" ]; then
        echo "release.sh: HEAD is not what tag $VERSION names; refusing to build a release from it" >&2
        exit 1
    fi
    command -v zig >/dev/null || { echo "release.sh: --from-tag builds the whole matrix and zig is missing (the linux cross C compiler)" >&2; exit 1; }
    command -v gh >/dev/null || { echo "release.sh: gh is required to publish" >&2; exit 1; }
fi

rm -rf "$DIST"
mkdir -p "$DIST"

build() {
    local goos="$1" goarch="$2" cc="${3:-}"
    local name="ragota_${VERSION}_${goos}_${goarch}"
    local out="$DIST/$name"
    echo "  $goos/$goarch"
    mkdir -p "$out"
    # CC defaults to the system compiler; a quoted assignment survives the
    # spaces in "zig cc -target …", which ${cc:+CC="$cc"} did not — that
    # word-split into CC=zig plus a command named cc.
    CGO_ENABLED=1 GOOS="$goos" GOARCH="$goarch" CC="${cc:-cc}" \
        go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out/ragota" ./cmd/ragota
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

PREV="$(git describe --tags --abbrev=0 "$VERSION^" 2>/dev/null || true)"
NOTES="$(git log --oneline ${PREV:+$PREV..}"$VERSION" | sed 's/^/- /')"
gh release create "$VERSION" "$DIST"/*.tar.gz "$DIST/checksums.txt" \
    --verify-tag --title "ragota $VERSION" --notes "$NOTES"
echo "published: $(gh release view "$VERSION" --json url -q .url)"
