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
#   - a Python traceback becomes "traceback: <its exception line>", with
#     addresses and numbers (ports, paths with timestamps) replaced by <ip> and
#     N, so a row excuses the one exception it documents and not any crash in
#     the same subtest. A message spanning several lines is named by its last
#     line, which then carries no "Exception: " prefix
#   - a leading "<subtest> - " is dropped; the name already carries it
#
# A module with no closing tally did not finish; it grades as a TIMEOUT named
# "<module>: did not finish", which no blacklist entry can excuse. A module
# whose tally disagrees with the PASS and FAIL lines parsed for it grades as
# INCOMPLETE: an assertion line the parser missed could be a failure, and
# grading only what it did read could turn a red run green.
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
    # A traceback is emitted once its last line is known: nfstest indents the
    # frames and the exception ten spaces or more, and the exception is the
    # last of those lines before the next normal one.
    function flush_traceback(    exc) {
        if (!in_tb) return
        in_tb = 0
        exc = tb_last
        gsub(/[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/, "<ip>", exc)
        gsub(/[0-9]+/, "N", exc)
        emit("FAIL", "traceback: " (exc == "" ? "?" : exc))
    }
    function flush_module() {
        flush_traceback()
        if (module == "") return
        if (!finished) printf "TIMEOUT\t%s: did not finish\n", module
        else if (passed != tally_pass || failed != tally_fail)
            printf "INCOMPLETE\t%s: tally says %d passed, %d failed; parsed %d, %d\n", module, tally_pass, tally_fail, passed, failed
    }
    function emit(verdict, msg,    name) {
        sub(/^[ \t]+/, "", msg)
        sub(/[ \t]+$/, "", msg)
        sub(/ \([0-9]+ passed, [0-9]+ failed\)$/, "", msg)
        # nfstest starts most messages with the subtest name, which the
        # name already carries.
        if (subtest != "" && index(msg, subtest " - ") == 1) msg = substr(msg, length(subtest) + 4)
        if (verdict == "PASS") passed++; else failed++
        gsub(/\|/, "+", msg)
        gsub(/[ \t]+/, " ", msg)
        name = module "/" (subtest == "" ? "-" : subtest) ": " msg
        printf "%s\t%s\n", verdict, name
    }
    in_tb && /^          / {
        line = $0; sub(/^[ \t]+/, "", line); sub(/[ \t]+$/, "", line)
        if (line != "") tb_last = line
        next
    }
    in_tb { flush_traceback() }
    /^NFSTEST-MODULE / {
        flush_module()
        module = $2; sub(/^nfstest_/, "", module)
        subtest = ""; finished = 0; passed = 0; failed = 0
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
    /^[ \t]*FAIL: Traceback \(most recent call last\)/ { in_tb = 1; tb_last = ""; next }
    /^[ \t]*FAIL: / { line = $0; sub(/^[ \t]*FAIL: /, "", line); emit("FAIL", line); next }
    /^[0-9]+ tests \([0-9]+ passed, [0-9]+ failed/ {
        finished = 1
        tally_pass = $3; sub(/^\(/, "", tally_pass); tally_pass += 0
        tally_fail = $5 + 0
        next
    }
' "$LOG" >"$WORK/results"

[[ -s "$WORK/results" ]] || ungraded "no PASS or FAIL lines in the log"

"$SCRIPT_DIR/../grade.sh" "$WORK/results" "$KNOWN_FAILURES_FILE" "$RESULTS_DIR"
