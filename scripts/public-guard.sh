#!/bin/sh
# public-guard: refuse to publish commits or tags whose metadata contains a
# denied string. The file tree is checked separately (TestNoPersonalStrings);
# this covers what a tree scan can't see: author/committer identities, commit
# messages, tag names, taggers and tag messages.
#
# Usage: GRAB_DENYLIST='a,b,c' scripts/public-guard.sh <rev-range|--all> [tag ...]
# Exit: 0 clean, 1 finding(s), 2 usage error / empty deny-list / bad range.
# Findings name the commit or tag and the field, never the matched value: CI
# logs are public enough that echoing the value would leak it.
set -eu

[ $# -ge 1 ] || { echo "usage: $0 <rev-range|--all> [tag ...]" >&2; exit 2; }
range=$1
shift

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# One entry per line, trimmed, blanks dropped.
printf '%s' "${GRAB_DENYLIST:-}" | tr ',' '\n' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | grep -v '^$' > "$tmp/deny" || true
if [ ! -s "$tmp/deny" ]; then
	echo "public-guard: GRAB_DENYLIST is empty; refusing to run" >&2
	exit 2
fi

found=0
# check <label> <field> <value>: no pipes into grep -q (pipefail-safe); the
# value goes through a file.
check() {
	printf '%s\n' "$3" > "$tmp/value"
	if grep -F -i -q -f "$tmp/deny" "$tmp/value"; then
		echo "public-guard: $1: $2"
		found=1
	fi
}

if [ "$range" = "--all" ]; then
	revs=$(git rev-list --all) || exit 2
else
	revs=$(git rev-list "$range" 2>/dev/null) || { echo "public-guard: cannot read range" >&2; exit 2; }
fi

for c in $revs; do
	short=$(git rev-parse --short "$c")
	check "commit $short" "author name" "$(git show -s --format=%an "$c")"
	check "commit $short" "author email" "$(git show -s --format=%ae "$c")"
	check "commit $short" "committer name" "$(git show -s --format=%cn "$c")"
	check "commit $short" "committer email" "$(git show -s --format=%ce "$c")"
	check "commit $short" "message" "$(git show -s --format=%B "$c")"
done

for t in "$@"; do
	git rev-parse -q --verify "refs/tags/$t" >/dev/null || { echo "public-guard: no such tag" >&2; exit 2; }
	# A denied tag name would leak through its own label: name it by object id.
	label="tag $t"
	before=$found
	check "tag $(git rev-parse --short "refs/tags/$t")" "name" "$t"
	[ "$found" = "$before" ] || label="tag $(git rev-parse --short "refs/tags/$t")"
	check "$label" "tagger name" "$(git for-each-ref --format='%(taggername)' "refs/tags/$t")"
	check "$label" "tagger email" "$(git for-each-ref --format='%(taggeremail)' "refs/tags/$t")"
	check "$label" "message" "$(git for-each-ref --format='%(contents)' "refs/tags/$t")"
done

exit $found
