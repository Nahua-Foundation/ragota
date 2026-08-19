#!/usr/bin/env bash
# Clone the benchmark corpus shallowly, at the commits repos.tsv pins.
#
# Usage: clone.sh [-d dir] [-l list] [name ...]
#
#   -d dir   where the checkouts go (default ./corpus, $CORPUS_DIR)
#   -l list  repository list (default repos.tsv next to this script)
#   name...  clone only these repositories (by name or by checkout directory)
#
# Existing checkouts are left alone; delete a directory to re-clone it. The
# clones are --depth 1: the corpus measures what the extractor sees in a tree,
# never history, and the full histories are tens of gigabytes.
#
# A checkout lands in the directory repos.tsv names, which is what the eval's
# ground truth calls it ("petclinic", not "spring-petclinic-microservices"),
# and at the commit repos.tsv pins. The pin is the point: ground truth pinned
# to file and line is only true of one tree, and HEAD is a different tree every
# week. A row with no commit falls back to HEAD and says so.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dir="${CORPUS_DIR:-$PWD/corpus}"
list="$here/repos.tsv"

while getopts ":d:l:h" opt; do
	case "$opt" in
		d) dir="$OPTARG" ;;
		l) list="$OPTARG" ;;
		h) sed -n '2,19p' "${BASH_SOURCE[0]}"; exit 0 ;;
		*) echo "unknown option -$OPTARG" >&2; exit 2 ;;
	esac
done
shift $((OPTIND - 1))

want=""
if [ "$#" -gt 0 ]; then
	want=" $* "
fi

mkdir -p "$dir"
failed=0
unpinned=0

# fetch_pinned checks out one commit without downloading the history around it:
# an empty repository, one remote, one depth-1 fetch of the commit itself. It is
# what makes a pinned corpus affordable — the alternative, cloning and then
# checking out, is the full history the shallow clone exists to avoid.
fetch_pinned() {
	local target="$1" url="$2" commit="$3"
	rm -rf "$target"
	git init --quiet "$target"
	git -C "$target" remote add origin "$url"
	if ! git -C "$target" fetch --quiet --depth 1 origin "$commit"; then
		rm -rf "$target"
		return 1
	fi
	git -C "$target" checkout --quiet FETCH_HEAD
}

# Fields are cut out one at a time rather than read into six variables at once:
# bash treats a tab as IFS whitespace, so two adjacent tabs collapse into one
# delimiter and every row with an empty column silently shifts. That produced a
# checkout directory named after a commit hash before it was caught.
while IFS= read -r line; do
	case "$line" in ''|\#*) continue ;; esac
	name=$(printf '%s' "$line" | cut -f1)
	url=$(printf '%s' "$line" | cut -f2)
	pattern=$(printf '%s' "$line" | cut -f3)
	stack=$(printf '%s' "$line" | cut -f4)
	checkout=$(printf '%s' "$line" | cut -f5)
	commit=$(printf '%s' "$line" | cut -f6)
	checkout="${checkout:-$name}"
	if [ -n "$want" ] && [[ "$want" != *" $name "* ]] && [[ "$want" != *" $checkout "* ]]; then
		continue
	fi
	target="$dir/$checkout"
	if [ -d "$target/.git" ]; then
		echo "have    $checkout"
		continue
	fi
	echo "clone   $checkout ($pattern, $stack)"
	if [ -n "$commit" ]; then
		if fetch_pinned "$target" "$url" "$commit"; then
			continue
		fi
		# Some servers refuse to serve an arbitrary commit. Falling back is
		# better than failing, but the tree is then not the one the ground
		# truth was written against, and that has to be said out loud.
		echo "WARNING $checkout — $url will not serve $commit; falling back to HEAD, which the eval's ground truth is not pinned to" >&2
		unpinned=$((unpinned + 1))
	else
		echo "WARNING $checkout — no commit pinned in the list; cloning HEAD" >&2
		unpinned=$((unpinned + 1))
	fi
	if ! git clone --depth 1 --quiet "$url" "$target"; then
		echo "FAILED  $checkout — $url" >&2
		failed=$((failed + 1))
	fi
done < "$list"

echo
du -sh "$dir" 2>/dev/null || true
if [ "$unpinned" -gt 0 ]; then
	echo "$unpinned checkout(s) are at HEAD rather than the pinned commit; run tools/eval/run.py --validate before trusting any number from them" >&2
fi
if [ "$failed" -gt 0 ]; then
	echo "$failed repositories failed to clone" >&2
	exit 1
fi
