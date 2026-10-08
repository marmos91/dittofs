#!/usr/bin/env bash
# Grade an nfstest log written by run.sh against a KNOWN_FAILURES table.
#
# Usage:
#   ./parse-results.sh <nfstest.log> <known-failures-file> [results-dir]
#
# run.sh brackets each module's output with "NFSTEST-MODULE <module>" and ends
# the log with "NFSTEST-DONE <number of modules>". Inside a module, nfstest
# prints "TEST: Running test '<subtest>'" before each subtest, then one
# "PASS: <message>" or "FAIL: <message>" per assertion, and closes with
# "<n> tests (<p> passed, <f> failed)".
#
# Each assertion is graded under the name "<module>/<subtest>: <message>", so a
# blacklist entry names one assertion rather than a whole subtest — a new
# failure inside a subtest that already has a known one still fails the run.
# Messages are normalised so the name is stable and fits a Markdown table:
#   - a trailing "(N passed, M failed)" tally is dropped; its numbers move
#   - "|" becomes "+", since it would end the table cell (O_EXCL|O_CREAT)
#   - a Python traceback becomes "traceback", whatever its frames say
#   - a leading "<subtest> - " is dropped; the name already carries it
#
# A module with no closing tally did not finish; it grades as a TIMEOUT named
# "<module>: did not finish", which no blacklist entry can excuse.
#
# Exit status: as test/nfs-mount/grade.sh, or 1 when the log is not gradable.

set -euo pipefail

LOG="${1:-}"
KNOWN_FAILURES_FILE="${2:-}"
RESULTS_DIR="${3:-}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ungraded() {
    echo "ERROR: $*" >&2
    [[ -n "$RESULTS_DIR" && -d "$RESULTS_DIR" ]] && echo "ungraded 0 0 0" >"$RESULTS_DIR/verdict"
    exit 1
}

[[ -f "$LOG" ]] || ungraded "log not found: ${LOG:-<unset>}"

done_line="$(grep -E '^NFSTEST-DONE [0-9]+$' "$LOG" | tail -1 || true)"
[[ -n "$done_line" ]] || ungraded "no NFSTEST-DONE marker — the run did not finish"
modules="$(grep -cE '^NFSTEST-MODULE [a-z0-9_]+$' "$LOG" || true)"
[[ "$modules" -eq "${done_line#NFSTEST-DONE }" ]] ||
    ungraded "log has $modules module(s) but the run reported ${done_line#NFSTEST-DONE }"
[[ "$modules" -gt 0 ]] || ungraded "the run reported no modules"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# POSIX awk only: this has to run on the BSD awk of a macOS checkout too.
awk '
    function flush_module() {
        if (module != "" && !finished) printf "TIMEOUT\t%s: did not finish\n", module
    }
    function emit(verdict, msg,    name) {
        sub(/^[ \t]+/, "", msg)
        sub(/[ \t]+$/, "", msg)
        sub(/ \([0-9]+ passed, [0-9]+ failed\)$/, "", msg)
        if (msg ~ /^Traceback \(most recent call last\)/) msg = "traceback"
        # nfstest starts most messages with the subtest name, which the
        # name already carries.
        if (subtest != "" && index(msg, subtest " - ") == 1) msg = substr(msg, length(subtest) + 4)
        gsub(/\|/, "+", msg)
        gsub(/[ \t]+/, " ", msg)
        name = module "/" (subtest == "" ? "-" : subtest) ": " msg
        printf "%s\t%s\n", verdict, name
    }
    /^NFSTEST-MODULE / {
        flush_module()
        module = $2; sub(/^nfstest_/, "", module)
        subtest = ""; finished = 0
        next
    }
    /^NFSTEST-DONE / { flush_module(); module = ""; next }
    module == "" { next }
    /^[ \t]*TEST: Running test / {
        subtest = $0
        sub(/^.*Running test \047/, "", subtest)
        sub(/\047.*$/, "", subtest)
        next
    }
    /^[ \t]*PASS: / { line = $0; sub(/^[ \t]*PASS: /, "", line); emit("PASS", line); next }
    /^[ \t]*FAIL: / { line = $0; sub(/^[ \t]*FAIL: /, "", line); emit("FAIL", line); next }
    /^[0-9]+ tests \([0-9]+ passed, [0-9]+ failed/ { finished = 1; next }
' "$LOG" >"$WORK/results"

[[ -s "$WORK/results" ]] || ungraded "no PASS or FAIL lines in the log"

"$SCRIPT_DIR/../grade.sh" "$WORK/results" "$KNOWN_FAILURES_FILE" "$RESULTS_DIR"
