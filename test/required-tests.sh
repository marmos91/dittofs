#!/usr/bin/env bash
#
# Runs every test named in required-tests.json and insists each one passed.
#
# Deleting a regression test is invisible: the suite that no longer contains it
# reports nothing missing, so removing the test and reintroducing the defect it
# pinned is a single green change. These names are checked by name instead, so
# the deletion has to be written down here as well to stay green.
#
# Running the tests rather than grepping for them is what makes "gone" and
# "quietly not running" the same answer — a name behind a build tag, renamed,
# skipped at runtime or failing all fail here, and only a real pass clears it.
#
# Usage: test/required-tests.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
MANIFEST="${SCRIPT_DIR}/required-tests.json"

cd "$REPO_ROOT"

status=0

while read -r pkg; do
	names=$(jq -r --arg p "$pkg" '.packages[$p][]' "$MANIFEST")
	# ^(A|B|C)$ so a name cannot be satisfied by another test containing it.
	pattern="^($(echo "$names" | paste -sd '|' -))$"

	echo "== ${pkg}"
	# -count=1 because a cached pass is evidence about an older tree.
	output=$(go test -count=1 -v -run "$pattern" "$pkg" 2>&1) || true

	missing=""
	while read -r name; do
		# Top-level results start at column 0; subtest lines are indented. The
		# trailing " (" stops a name matching a longer one that contains it.
		if ! grep -q "^--- PASS: ${name} (" <<<"$output"; then
			missing+="  ${name}"$'\n'
		fi
	done <<<"$names"

	if [ -n "$missing" ]; then
		echo "$output" >&2
		echo >&2
		echo "these required tests did not pass in ${pkg}:" >&2
		printf '%s' "$missing" >&2
		status=1
	fi
done < <(jq -r '.packages | keys[]' "$MANIFEST")

if [ "$status" -ne 0 ]; then
	echo >&2
	echo "A name above is missing, renamed, skipped or failing. These tests pin" >&2
	echo "authorization and encryption gates whose removal is otherwise silent." >&2
	echo >&2
	echo "If the test moved or was renamed, update test/required-tests.json in the" >&2
	echo "same change. If it should no longer exist, delete the entry there and say" >&2
	echo "in the commit message what now covers the gate." >&2
	exit 1
fi

echo
echo "every required test ran and passed"
