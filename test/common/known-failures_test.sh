#!/usr/bin/env bash
# Unit tests for the shared known-failures (blacklist) parser.
#
# Every conformance suite grades against a table parsed by kf_load, so a
# mis-parse here is silent in every suite at once. The asymmetry is what makes
# this worth pinning: a row DROPPED makes a known failure look new and fails in
# the hour, while a row ADDED excuses a test that is currently passing and
# reports nothing at all. So each table must load exactly its expected rows.
#
# The cases live in testdata/, which the Go matcher in test/harness/known runs
# too, so the two can't drift:
#   - NAME.md is a table, and NAME.want what kf_load loads from it: one
#     "name<TAB>reason" line per row, sorted bytewise.
#   - match.txt holds "pattern<TAB>name<TAB>yes|no" lines: whether the name
#     matches a list holding only the pattern.
#
# Usage: ./known-failures_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./known-failures.sh
source "${SCRIPT_DIR}/known-failures.sh"
TESTDATA="${SCRIPT_DIR}/testdata"

FAILURES=0
ASSERTIONS=0

ok()   { ASSERTIONS=$((ASSERTIONS + 1)); echo "ok: $1"; }
fail() { FAILURES=$((FAILURES + 1));     echo "FAIL: $1"; }

# assert_eq NAME EXPECTED ACTUAL
assert_eq() {
    if [[ "$2" == "$3" ]]; then ok "$1"; else fail "$1: expected '$2', got '$3'"; fi
}

reset_kf() {
    unset KF_KNOWN KF_REASON
    # shellcheck disable=SC2034  # populated and consumed by kf_load
    declare -gA KF_KNOWN KF_REASON
    KF_COUNT=0
}

# rows FILE prints what kf_load loads from FILE, as the .want files hold it.
rows() {
    reset_kf
    kf_load "$1"
    local name
    for name in "${!KF_KNOWN[@]}"; do
        printf '%s\t%s\n' "$name" "$(kf_reason "$name")"
    done | LC_ALL=C sort
}

# Each table loads exactly its expected rows, and KF_COUNT counts them.
for table in "$TESTDATA"/*.md; do
    case_name="$(basename "$table" .md)"
    want="$(cat "${table%.md}.want")"
    got="$(rows "$table")"
    if [[ "$got" == "$want" ]]; then
        ok "${case_name}: loads exactly its rows"
    else
        fail "${case_name}: the rows differ (< want, > got)"
        diff <(printf '%s\n' "$want") <(printf '%s\n' "$got")
    fi
    reset_kf
    kf_load "$table"
    assert_eq "${case_name}: KF_COUNT counts the rows" "$(grep -c . "${table%.md}.want")" "$KF_COUNT"
done

# Each pattern matches the names it should and no others.
while IFS=$'\t' read -r pattern name want; do
    [[ -z "$pattern" || "$pattern" == \#* ]] && continue
    reset_kf
    KF_KNOWN["$pattern"]=1
    if kf_is_known "$name"; then got=yes; else got=no; fi
    assert_eq "'${pattern}' against '${name}'" "$want" "$got"
done < "${TESTDATA}/match.txt"

echo
echo "assertions: ${ASSERTIONS}, failures: ${FAILURES}"
[[ "$FAILURES" -eq 0 ]]
