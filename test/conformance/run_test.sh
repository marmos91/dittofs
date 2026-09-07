#!/usr/bin/env bash
# Unit tests for the common conformance runner.
#
# Everything here runs against a synthetic manifest and synthetic step scripts,
# so no server, no Docker, no mount and no root — in about a second. The things
# worth pinning are the ones a plausible refactor would quietly destroy: the
# grader's failure COUNT surviving to the caller, teardown running after a
# failure, and privilege being decided per step rather than for the whole run.
#
# Usage: ./run_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUNNER="${SCRIPT_DIR}/run.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILURES=0
ASSERTIONS=0

ok() {
    ASSERTIONS=$((ASSERTIONS + 1))
    echo "ok: $1"
}

fail() {
    FAILURES=$((FAILURES + 1))
    echo "FAIL: $1"
}

# assert_eq NAME EXPECTED ACTUAL
assert_eq() {
    if [[ "$2" == "$3" ]]; then
        ok "$1"
    else
        fail "$1: expected '$2', got '$3'"
    fi
}

# assert_contains NAME NEEDLE HAYSTACK
assert_contains() {
    if [[ "$3" == *"$2"* ]]; then
        ok "$1"
    else
        fail "$1: output missing '$2'"
        printf '%s\n' "$3" | sed 's/^/    /'
    fi
}

# assert_not_contains NAME NEEDLE HAYSTACK
assert_not_contains() {
    if [[ "$3" != *"$2"* ]]; then
        ok "$1"
    else
        fail "$1: output unexpectedly contains '$2'"
    fi
}

# ---------------------------------------------------------------------------
# A synthetic suite tree: test/-shaped, so step commands resolve the same way
# they do in the repo.
# ---------------------------------------------------------------------------
FAKE_TEST="${WORK}/test"
mkdir -p "${FAKE_TEST}/conformance" "${FAKE_TEST}/fake"
FAKE_MANIFEST="${FAKE_TEST}/conformance/suites.json"

: >"${FAKE_TEST}/fake/KNOWN_FAILURES_A.md"
: >"${FAKE_TEST}/fake/KNOWN_FAILURES_B.md"

cat >"${FAKE_TEST}/fake/pass.sh" <<'EOF'
#!/usr/bin/env bash
echo "ran pass.sh profile=${DITTOFS_PROFILE} variant=${DITTOFS_VARIANT} kf=$(basename "${DITTOFS_KNOWN_FAILURES:-none}")"
echo "args: $*"
exit 0
EOF

# Stands in for a grader: exits with the number of new failures.
cat >"${FAKE_TEST}/fake/fail3.sh" <<'EOF'
#!/usr/bin/env bash
echo "3 new failures"
exit 3
EOF

cat >"${FAKE_TEST}/fake/teardown.sh" <<'EOF'
#!/usr/bin/env bash
echo "TEARDOWN RAN"
exit 0
EOF

chmod +x "${FAKE_TEST}"/fake/*.sh

cat >"$FAKE_MANIFEST" <<'EOF'
{
  "defaults": { "tiers": { "pull_request": "all", "push": "all" } },
  "suites": {
    "green": {
      "description": "always passes",
      "runner_dir": "fake",
      "profiles": ["memory", "badger"],
      "known_failures": "fake/KNOWN_FAILURES_A.md",
      "tiers": { "pull_request": ["memory"], "push": "all" },
      "steps": [
        { "name": "run", "cmd": "fake/pass.sh", "args": ["--profile", "{profile}"], "root": false }
      ]
    },
    "graded": {
      "description": "fails with a count, and tears down either way",
      "runner_dir": "fake",
      "profiles": ["memory"],
      "variant": { "name": "minor-version", "values": ["4.0", "4.1"] },
      "known_failures": {
        "4.0": "fake/KNOWN_FAILURES_A.md",
        "4.1": "fake/KNOWN_FAILURES_B.md"
      },
      "tiers": { "pull_request": "all", "push": "all" },
      "steps": [
        { "name": "run", "cmd": "fake/fail3.sh", "args": [], "root": false },
        { "name": "grade", "cmd": "fake/pass.sh", "args": [], "root": false },
        { "name": "teardown", "cmd": "fake/teardown.sh", "args": [], "root": false, "always": true }
      ]
    },
    "privileged": {
      "description": "one step needs root, one does not",
      "runner_dir": "fake",
      "profiles": ["memory"],
      "known_failures": null,
      "tiers": { "pull_request": "all", "push": "all" },
      "steps": [
        { "name": "setup", "cmd": "fake/pass.sh", "args": [], "root": true },
        { "name": "run", "cmd": "fake/pass.sh", "args": [], "root": false }
      ]
    }
  }
}
EOF

run_fake() {
    CONFORMANCE_MANIFEST="$FAKE_MANIFEST" "$RUNNER" --results-dir "${WORK}/results" "$@" 2>&1
}

# ---------------------------------------------------------------------------
# The real manifest describes the repo it ships with.
# ---------------------------------------------------------------------------
REAL_LIST="$("$RUNNER" --list 2>&1)"
for suite in wpts smbtorture pjdfstest nfs-kerberos pynfs; do
    assert_contains "manifest lists ${suite}" "$suite" "$REAL_LIST"
done

# Every step command the manifest names has to exist and be runnable, or the
# manifest is describing a tree that is not there.
MISSING=""
while IFS= read -r cmd; do
    [[ -x "${SCRIPT_DIR}/../${cmd}" ]] || MISSING="${MISSING} ${cmd}"
done < <(jq -r '.suites[].steps[].cmd' "${SCRIPT_DIR}/suites.json")
assert_eq "every declared step command is executable" "" "$MISSING"

# Same for the blacklists.
MISSING=""
while IFS= read -r kf; do
    [[ -f "${SCRIPT_DIR}/../${kf}" ]] || MISSING="${MISSING} ${kf}"
done < <(jq -r '[.suites[].known_failures | select(. != null) | if type == "string" then . else .[] end] | unique[]' "${SCRIPT_DIR}/suites.json")
assert_eq "every declared blacklist exists" "" "$MISSING"

# The CI matrix is the manifest's cross product, not a hand-kept list.
PR_MATRIX="$("$RUNNER" --matrix pull_request)"
assert_eq "presubmit matrix size" \
    "$(jq -r '[.suites | to_entries[] | (.value.tiers.pull_request // "all") as $t
              | (if $t == "all" then (.value.profiles | length) else ($t | length) end)
                * (.value.variant.values // [""] | length)] | add' "${SCRIPT_DIR}/suites.json")" \
    "$(jq -r '.include | length' <<<"$PR_MATRIX")"
assert_contains "presubmit matrix carries the variant axis" '"variant":"4.1"' "$PR_MATRIX"

# ---------------------------------------------------------------------------
# Verdicts
# ---------------------------------------------------------------------------
OUT="$(run_fake --suite green --profile memory)"
assert_eq "a green suite exits 0" "0" "$?"
assert_contains "the step actually ran" "ran pass.sh profile=memory" "$OUT"
assert_contains "profile placeholder is substituted" "args: --profile memory" "$OUT"

# The whole point of the exit contract: 3 new failures must arrive as 3, not 1.
OUT="$(run_fake --suite graded --profile memory --variant 4.0)"
rc=$?
assert_eq "a grader's failure count survives to the caller" "3" "$rc"
assert_contains "teardown runs after a failed step" "TEARDOWN RAN" "$OUT"
assert_not_contains "steps after a failure are skipped" "ran pass.sh" "$OUT"

OUT="$(run_fake --suite graded --profile memory --variant 4.0 --keep)"
assert_not_contains "--keep skips teardown" "TEARDOWN RAN" "$OUT"

# ---------------------------------------------------------------------------
# Per-variant blacklists. One path per suite cannot express these.
# ---------------------------------------------------------------------------
OUT="$(run_fake --suite graded --profile memory --variant 4.1)"
assert_contains "4.1 grades against its own table" "KNOWN_FAILURES_B.md" "$OUT"
OUT="$(run_fake --suite graded --profile memory --variant 4.0)"
assert_contains "4.0 grades against its own table" "KNOWN_FAILURES_A.md" "$OUT"

# ---------------------------------------------------------------------------
# Privilege is a property of a step, not of a run. pynfs is its own NFSv4
# client and has to stay runnable with no mount and no root.
# ---------------------------------------------------------------------------
OUT="$(run_fake --suite privileged --profile memory --dry-run)"
assert_contains "a root step is elevated" "setup: sudo -E" "$OUT"
assert_not_contains "a non-root step is not" "run: sudo -E" "$OUT"

OUT="$("$RUNNER" --suite pynfs --profile memory --variant 4.1 --dry-run 2>&1)"
assert_not_contains "no step of pynfs asks for root" "sudo" "$OUT"
assert_contains "pynfs pins its minor version" "--minor-version 4.1" "$OUT"

# ---------------------------------------------------------------------------
# Refusals
# ---------------------------------------------------------------------------
OUT="$(run_fake --suite nope 2>&1)"
assert_eq "an unknown suite is refused" "2" "$?"
assert_contains "the refusal names the suite" "unknown suite: nope" "$OUT"

OUT="$(run_fake --suite green --profile nope 2>&1)"
assert_eq "an unknown profile is refused" "2" "$?"

OUT="$(run_fake --suite graded --profile memory --variant 9.9 2>&1)"
assert_eq "an unknown variant is refused" "2" "$?"

OUT="$(run_fake --suite green --profile memory --variant 4.0 2>&1)"
assert_eq "a variant on a suite that has no variant axis is refused" "2" "$?"

OUT="$(run_fake --suite green --profile memory --tier push 2>&1)"
assert_eq "--profile and --tier together are refused" "2" "$?"

# A tier runs every profile that tier declares.
OUT="$(run_fake --suite green --tier push)"
assert_eq "a tier run covers every profile" "2" \
    "$(grep -c 'ran pass.sh' <<<"$OUT")"

echo ""
if [[ "$ASSERTIONS" -eq 0 ]]; then
    echo "FAIL: no assertions ran"
    exit 1
fi
if [[ "$FAILURES" -eq 0 ]]; then
    echo "PASS: ${ASSERTIONS} assertions"
    exit 0
fi
echo "FAIL: ${FAILURES} of ${ASSERTIONS} assertions failed"
exit 1
