#!/usr/bin/env bash
# Clone the benchmark corpus shallowly.
#
# Usage: clone.sh [-d dir] [-l list] [name ...]
#
#   -d dir   where the checkouts go (default ./corpus, $CORPUS_DIR)
#   -l list  repository list (default repos.tsv next to this script)
#   name...  clone only these repositories
#
# Existing checkouts are left alone; delete a directory to re-clone it. The
# clones are --depth 1: the corpus measures what the extractor sees in a tree,
# never history, and the full histories are tens of gigabytes.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dir="${CORPUS_DIR:-$PWD/corpus}"
list="$here/repos.tsv"

while getopts ":d:l:h" opt; do
	case "$opt" in
		d) dir="$OPTARG" ;;
		l) list="$OPTARG" ;;
		h) sed -n '2,14p' "${BASH_SOURCE[0]}"; exit 0 ;;
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

while IFS=$'\t' read -r name url pattern stack; do
	case "$name" in ''|\#*) continue ;; esac
	if [ -n "$want" ] && [[ "$want" != *" $name "* ]]; then
		continue
	fi
	target="$dir/$name"
	if [ -d "$target/.git" ]; then
		echo "have    $name"
		continue
	fi
	echo "clone   $name ($pattern, $stack)"
	if ! git clone --depth 1 --quiet "$url" "$target"; then
		echo "FAILED  $name — $url" >&2
		failed=$((failed + 1))
	fi
done < "$list"

echo
du -sh "$dir" 2>/dev/null || true
if [ "$failed" -gt 0 ]; then
	echo "$failed repositories failed to clone" >&2
	exit 1
fi
