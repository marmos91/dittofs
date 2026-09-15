#!/usr/bin/env bash
#
# A commit that calls itself documentation changes documentation.
#
# `docs:` and `chore(release):` are the two types that say, in the subject line,
# that nothing executable moved. A reviewer reads the subject and skips the
# diff, so a commit that claims one of them and carries Go source or a workflow
# is the one change nobody looks at. That is not hypothetical: a two-line
# ROADMAP edit titled `docs(planning):` once carried 830 deletions across 15
# files, reverted two merged security fixes, and stayed green because the
# regression tests went with them.
#
# Allowed paths for those two types: docs/**, .planning/**, any *.md, README*.
#
# The one exception is a Go file whose diff touches only `//` comment lines and
# blank lines — a typo fix in a doc comment is a documentation change whatever
# file it lives in. The exception applies to modified files only: an added,
# deleted or renamed .go file is a structural change regardless of its
# contents, which is precisely the case above. Block comments (/* */) are not
# recognised, so a typo fix inside one fails closed and wants a different type.
#
# Merge commits carry no meaningful type and are skipped. So are reverts: the
# type in `Revert "docs(x): y"` describes the commit being undone, not this
# diff, and it does not parse as a conventional type anyway.
#
# Usage: test/commit-type-matches-diff.sh [base-ref]   (default: origin/develop)

set -euo pipefail

base="${1:-origin/develop}"

if ! merge_base=$(git merge-base "$base" HEAD 2>/dev/null); then
	echo "cannot find a merge base with ${base}: fetch it first (fetch-depth: 0)" >&2
	exit 2
fi

# Paths a documentation commit is allowed to touch.
is_doc_path() {
	case "$1" in
	docs/* | .planning/* | *.md | README*) return 0 ;;
	*) return 1 ;;
	esac
}

# True when every changed line in this file is a // comment or blank. Context
# lines are excluded by --unified=0; the +++/--- headers are dropped by name.
go_comments_only() {
	! git show --format='' --unified=0 "$1" -- "$2" |
		grep -E '^[+-]' |
		grep -Ev '^(\+\+\+|---)' |
		sed -E 's/^[+-][[:space:]]*//' |
		grep -qvE '^(//|$)'
}

status=0

for sha in $(git rev-list --no-merges "${merge_base}..HEAD"); do
	# First physical line only. %s folds a subject that wraps before the
	# first blank line into one line, which would hide a second line.
	subject=$(git show -s --format=%B "$sha" | head -1)

	# Conventional type and optional scope: "type(scope)!: summary".
	if [[ ! "$subject" =~ ^([a-z]+)(\(([^\)]*)\))?!?: ]]; then
		continue
	fi
	type="${BASH_REMATCH[1]}"
	scope="${BASH_REMATCH[3]:-}"

	if [ "$type" != "docs" ] && [ "$type$scope" != "chorerelease" ]; then
		continue
	fi

	offenders=""
	while IFS=$'\t' read -r st path rename_to; do
		[ -n "$path" ] || continue
		# Renames report the destination in a third field; judge that.
		[ "${st:0:1}" = "R" ] && path="$rename_to"
		is_doc_path "$path" && continue
		if [ "${path##*.}" = "go" ] && [ "$st" = "M" ] && go_comments_only "$sha" "$path"; then
			continue
		fi
		offenders+="  ${st}  ${path}"$'\n'
	done < <(git show --name-status --format='' "$sha")

	if [ -n "$offenders" ]; then
		echo "${sha:0:9} ${subject}" >&2
		printf '%s' "$offenders" >&2
		status=1
	fi
done

if [ "$status" -ne 0 ]; then
	echo >&2
	echo "A docs: or chore(release): commit may only touch docs/**, .planning/**," >&2
	echo "*.md or README*. A modified .go file is also allowed when its diff changes" >&2
	echo "only // comment and blank lines." >&2
	echo >&2
	echo "Retype the commit for what it actually changes, or split the code out of it." >&2
	exit 1
fi

echo "every docs: and chore(release): commit touches documentation only"
