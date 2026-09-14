#!/usr/bin/env bash
#
# A new Go package arrives with a test file, or it does not arrive.
#
# Splitting a package is how a god object gets taken apart, and the split is
# only real if each piece can be tested on its own. A new directory with no
# _test.go is a piece that was moved but never separated: its behaviour is
# still pinned by whatever tests stayed behind in the package it left, which is
# exactly the coupling the split was meant to remove.
#
# Only directories that did not exist on the base branch are checked, so the
# packages that predate this guard are left alone.
#
# Usage: test/new-package-tests.sh [base-ref]   (default: origin/develop)

set -euo pipefail

base="${1:-origin/develop}"

if ! merge_base=$(git merge-base "$base" HEAD 2>/dev/null); then
	echo "cannot find a merge base with ${base}: fetch it first (fetch-depth: 0)" >&2
	exit 2
fi

# Directories holding Go files in a given tree, one per line. A directory with
# no Go files is not a package and never reaches this list.
go_dirs() {
	git ls-tree -r --name-only "$1" | sed -n 's![^/]*\.go$!!p' | sed 's!/$!!' | sort -u
}

# Directories holding at least one Go test file.
test_dirs() {
	git ls-tree -r --name-only "$1" | sed -n 's![^/]*_test\.go$!!p' | sed 's!/$!!' | sort -u
}

new_dirs=$(comm -13 <(go_dirs "$merge_base") <(go_dirs HEAD))

if [ -z "$new_dirs" ]; then
	echo "no new Go packages in this change"
	exit 0
fi

tested=$(test_dirs HEAD)
untested=$(comm -23 <(echo "$new_dirs") <(echo "$tested"))

echo "new Go packages in this change:"
echo "$new_dirs" | sed 's/^/  /'

if [ -n "$untested" ]; then
	echo
	echo "these new packages carry no _test.go:" >&2
	echo "$untested" | sed 's/^/  /' >&2
	echo
	echo "Add a test that exercises the package through its own exported surface." >&2
	echo "If the behaviour is still covered only by tests left behind in the package" >&2
	echo "it was split out of, the split has not separated anything yet." >&2
	exit 1
fi

echo
echo "every new package carries a test"
