#!/usr/bin/env bash
# Unit tests for the shared known-failures (blacklist) parser.
#
# Every conformance suite grades against a table parsed by kf_load, so a
# mis-parse here is silent in every suite at once. The asymmetry is what makes
# this worth pinning: a row DROPPED makes a known failure look new and fails in
# the hour, while a row ADDED excuses a test that is currently passing and
# reports nothing at all. The assertions below are weighted toward the quiet
# direction.
#
# Usage: ./known-failures_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./known-failures.sh
source "${SCRIPT_DIR}/known-failures.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILURES=0
ASSERTIONS=0

ok()   { ASSERTIONS=$((ASSERTIONS + 1)); echo "ok: $1"; }
fail() { FAILURES=$((FAILURES + 1));     echo "FAIL: $1"; }

# assert_known NAME TESTNAME
assert_known() {
    if kf_is_known "$2"; then ok "$1"; else fail "$1: '$2' should be known"; fi
}

# assert_not_known NAME TESTNAME
assert_not_known() {
    if kf_is_known "$2"; then fail "$1: '$2' should NOT be known"; else ok "$1"; fi
}

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

# ---------------------------------------------------------------------------
# A table shaped like the real ones: a prose header, a fenced worked example
# that CONTAINS a table row, then the actual table.
# ---------------------------------------------------------------------------
cat > "${WORK}/fenced.md" <<'MD'
# Known failures

## Removing a row

A row may only be removed on the literal verdict line from the artifact:

```
WRT2 st_write.testSizes : PASS
| WRT2 | bug | example row inside a fence | #1234 |
```

## Expected Failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| WRT18     | bug      | change attr frozen across a write | #2382 |
| EID6g     | bug      | suffixed code, not just digits    | #2340 |
| CSESS16a  | bug      | another suffixed code             | #2340 |
| flock/*   | proto    | NLM not implemented               | -     |
MD

reset_kf
kf_load "${WORK}/fenced.md"

# The quiet direction: nothing inside the fence may become a row.
assert_not_known "a table row inside a fence is not loaded" "WRT2"

# The loud direction: real rows still load.
assert_known "a plain row loads"                    "WRT18"
assert_eq    "the row's reason is read from field 4" \
             "change attr frozen across a write" "$(kf_reason WRT18)"

# EID6g / CSESS16a separate "splits the row" from "matches the name's shape".
# A pattern like ^\| [A-Z]+[0-9]+ \| drops both; kf_load must not.
assert_known "a letter-suffixed code loads (EID6g)"    "EID6g"
assert_known "a letter-suffixed code loads (CSESS16a)" "CSESS16a"

assert_eq "the fence contributes no rows to the count" "4" "$KF_COUNT"

# Globs still work alongside all of the above.
assert_known "a glob row still matches" "flock/01.t"

# ---------------------------------------------------------------------------
# ~~~ is the other fence marker Markdown accepts, and fences may be indented.
# ---------------------------------------------------------------------------
cat > "${WORK}/tilde.md" <<'MD'
| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| REAL1     | bug      | a real row | - |

~~~
| TILDE1 | bug | inside a tilde fence | - |
~~~

  ```
  | INDENTED1 | bug | inside an indented fence | - |
  ```
MD

reset_kf
kf_load "${WORK}/tilde.md"
assert_known     "a row outside every fence loads"      "REAL1"
assert_not_known "a ~~~ fence is honoured"              "TILDE1"
assert_not_known "an indented fence is honoured"        "INDENTED1"
assert_eq        "only the real row counted"    "1" "$KF_COUNT"

# ---------------------------------------------------------------------------
# An unterminated fence must not swallow the rest of the file silently... but
# it must also not leak. Markdown treats EOF as closing it; what matters here
# is that rows before it survive.
# ---------------------------------------------------------------------------
cat > "${WORK}/unterminated.md" <<'MD'
| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| BEFORE1   | bug      | before the fence | - |

```
| AFTER1 | bug | after an unterminated fence | - |
MD

reset_kf
kf_load "${WORK}/unterminated.md"
assert_known     "rows before an unterminated fence survive" "BEFORE1"
assert_not_known "an unterminated fence still suppresses"    "AFTER1"

# ---------------------------------------------------------------------------
# A language tag on the fence must not defeat it.
# ---------------------------------------------------------------------------
cat > "${WORK}/tagged.md" <<'MD'
| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| KEEP1     | bug      | real | - |

```bash
| TAGGED1 | bug | inside a tagged fence | - |
```
MD

reset_kf
kf_load "${WORK}/tagged.md"
assert_known     "a row outside a tagged fence loads" "KEEP1"
assert_not_known 'a fence with a language tag is honoured' "TAGGED1"

echo
echo "assertions: ${ASSERTIONS}, failures: ${FAILURES}"
[[ "$FAILURES" -eq 0 ]]
