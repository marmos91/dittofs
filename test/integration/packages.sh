#!/usr/bin/env bash
# packages.sh: prints, one per line, the import paths whose test set the
# `integration` build tag changes. CI's integration job and the local harness
# (test/harness/bin/dt integration) both run exactly this list.
#
# The list is derived, never written down: a hand-maintained one goes stale the
# first time someone adds a tagged file to a package not on it, and the suite
# then stays green while it stops running the new tests. The comparison is on
# each package's test-file list rather than its count, so a tag that swaps one
# file for another is caught as well as one that adds.
#
# decision: the integration job runs these packages only, not ./... . The tag
# changes nothing about the rest, so running them there was duplicated work, but
# it also means that job is no longer a second, unshortened run of the whole
# suite. The unit job passes -short on a pull request, so a short-guarded test
# keeps its full-strength PR run only where this list happens to cover its
# package. Widen it back to ./... if a defect ever reaches develop that the full
# PR-time run would have caught.
#
# Exits 1 when it derives no package: either the tag is gone from the tree or the
# derivation is broken, and running nothing while reporting success is the one
# outcome a caller must never get.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

# Materialised to files rather than compared through process substitution: a
# `go list` failure inside <(...) is invisible, and an empty list would then read
# as "no packages to test" and pass.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
go list -f '{{.ImportPath}} {{.TestGoFiles}} {{.XTestGoFiles}}' ./... | sort >"$tmp/plain"
go list -tags=integration -f '{{.ImportPath}} {{.TestGoFiles}} {{.XTestGoFiles}}' ./... | sort >"$tmp/tagged"
comm -13 "$tmp/plain" "$tmp/tagged" | awk '{print $1}' | sort -u >"$tmp/pkgs"

count="$(wc -l <"$tmp/pkgs" | tr -d ' ')"
if [[ "$count" -eq 0 ]]; then
    echo "derived no integration-tagged packages: either the tag is gone from the tree or this derivation is broken" >&2
    exit 1
fi
echo "the integration tag changes the test set of ${count} package(s)" >&2
cat "$tmp/pkgs"
