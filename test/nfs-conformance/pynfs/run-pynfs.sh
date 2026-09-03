#!/usr/bin/env bash
# Run the pynfs NFSv4 protocol conformance suite against DittoFS.
#
# pynfs is its own NFSv4 client: it speaks the protocol over TCP, so unlike
# pjdfstest it needs no kernel mount and no privilege. What it exercises is the
# part POSIX suites cannot see — state, sessions, owners, locking and wire error
# codes.
#
# The server is provisioned by test/posix/setup-posix.sh --no-mount, which owns
# the backend profiles (memory, badger, postgres, postgres-s3). This suite
# deliberately reuses it rather than growing a second copy that would drift.
#
# Usage:
#   ./run-pynfs.sh [--profile memory] [--minor-version 4.0|4.1] [OPTIONS]

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

PROFILE="${PROFILE:-memory}"
MINOR_VERSION="4.0"
SERVER="localhost:12049"
EXPORT_PATH="/export"
TESTS="all"
NO_SETUP=false
KEEP=false
VERBOSE=false
# pynfs authenticates with AUTH_SYS. The export squashes root to admin
# (setup-posix.sh: share nfs-config set /export --squash root_to_admin), which
# is the same identity pjdfstest runs as, so uid 0 gets a usable export root.
TEST_UID=0
TEST_GID=0

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'
log()       { echo -e "${GREEN}[PYNFS]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[PYNFS]${NC} $*"; }
log_error() { echo -e "${RED}[PYNFS]${NC} $*" >&2; }

usage() {
    cat <<EOF
Usage: run-pynfs.sh [OPTIONS]

Run the pynfs NFSv4 protocol conformance suite against DittoFS and grade the
result against the version's KNOWN_FAILURES table.

Options:
  --profile PROFILE        Storage profile (default: memory)
                           Valid: memory badger postgres postgres-s3
                           Passed through to test/posix/setup-posix.sh.
  --minor-version VERSION  NFSv4 minor version (default: 4.0)
                           Valid: 4.0, 4.1
  --server HOST:PORT       Server to test (default: localhost:12049)
  --export PATH            Export path on the server (default: /export)
  --tests "CODES"          pynfs test codes or flags (default: all)
                           e.g. --tests "LOOK1 OPEN4", --tests lookup
  --no-setup               Do not start or tear down a server; test whatever is
                           already listening on --server
  --keep                   Leave the server running after the run
  --verbose                Stream pynfs output as it runs
  --help                   Show this help

Examples:
  ./run-pynfs.sh --profile memory --minor-version 4.0
  ./run-pynfs.sh --minor-version 4.1 --tests "SEQ1 SEQ2" --verbose
  ./run-pynfs.sh --no-setup --server 10.0.0.5:2049
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --profile)        PROFILE="$2"; shift 2 ;;
        --minor-version)  MINOR_VERSION="$2"; shift 2 ;;
        --server)         SERVER="$2"; shift 2 ;;
        --export)         EXPORT_PATH="$2"; shift 2 ;;
        --tests)          TESTS="$2"; shift 2 ;;
        --no-setup)       NO_SETUP=true; shift ;;
        --keep)           KEEP=true; shift ;;
        --verbose|-v)     VERBOSE=true; shift ;;
        --help|-h)        usage; exit 0 ;;
        *)
            log_error "Unknown option: $1"
            echo "Run with --help for usage."
            exit 1
            ;;
    esac
done

case "$MINOR_VERSION" in
    4.0) PYNFS_TREE="4.0"; MINOR_NUM=0; KNOWN_FAILURES="${SCRIPT_DIR}/KNOWN_FAILURES_V40.md" ;;
    4.1) PYNFS_TREE="4.1"; MINOR_NUM=1; KNOWN_FAILURES="${SCRIPT_DIR}/KNOWN_FAILURES_V41.md" ;;
    *)
        log_error "Invalid --minor-version '$MINOR_VERSION'. Valid values: 4.0, 4.1"
        exit 1
        ;;
esac

# --------------------------------------------------------------------------
# Locate pynfs.
# --------------------------------------------------------------------------
PYNFS_BIN="pynfs-${PYNFS_TREE}"
if ! command -v "$PYNFS_BIN" >/dev/null 2>&1; then
    if ! command -v nix >/dev/null 2>&1; then
        log_error "pynfs-${PYNFS_TREE} is not on PATH and nix is unavailable."
        log_error "Enter the dev shell (nix develop) or build it: nix build .#pynfs"
        exit 1
    fi
    log "Building pynfs via nix..."
    PYNFS_OUT="$(nix build "${REPO_ROOT}#pynfs" --no-link --print-out-paths)" || {
        log_error "nix build .#pynfs failed"
        exit 1
    }
    PYNFS_BIN="${PYNFS_OUT}/bin/pynfs-${PYNFS_TREE}"
fi

# --------------------------------------------------------------------------
# Server lifecycle.
# --------------------------------------------------------------------------
SETUP_SCRIPT="${REPO_ROOT}/test/posix/setup-posix.sh"
TEARDOWN_SCRIPT="${REPO_ROOT}/test/posix/teardown-posix.sh"
SERVER_STARTED=false

# teardown-posix.sh unmounts, so it insists on root. Nothing was mounted here,
# so fall back to stopping the server directly rather than blocking on a sudo
# password prompt on a developer machine.
cleanup() {
    [[ "$SERVER_STARTED" == true && "$KEEP" != true ]] || return 0
    log "Stopping DittoFS..."
    if [[ $EUID -eq 0 ]]; then
        "$TEARDOWN_SCRIPT" >/dev/null 2>&1 && return 0
    elif sudo -n true 2>/dev/null; then
        sudo "$TEARDOWN_SCRIPT" >/dev/null 2>&1 && return 0
    fi
    "${REPO_ROOT}/dfs" stop --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

if [[ "$NO_SETUP" != true ]]; then
    log "Starting DittoFS (profile: ${PROFILE})..."
    # setup-posix.sh --no-mount needs no privilege, but the teardown it pairs
    # with removes /tmp state that a previous sudo run may own.
    if ! "$SETUP_SCRIPT" "$PROFILE" --no-mount; then
        log_error "Server setup failed. See /tmp/dittofs-posix-server.log"
        exit 1
    fi
    SERVER_STARTED=true
fi

# --------------------------------------------------------------------------
# Run.
# --------------------------------------------------------------------------
RESULTS_DIR="${SCRIPT_DIR}/results/pynfs-v${MINOR_VERSION}-${PROFILE}-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_DIR"
LOG_FILE="${RESULTS_DIR}/pynfs.log"

# --minorversion is explicit for both trees: the 4.1 tree defaults to 2, so
# omitting it silently tests NFSv4.2 instead.
PYNFS_ARGS=(
    "${SERVER}${EXPORT_PATH}"
    --minorversion "$MINOR_NUM"
    --security sys
    --uid "$TEST_UID"
    --gid "$TEST_GID"
    --maketree
    --json "${RESULTS_DIR}/pynfs.json"
)
# shellcheck disable=SC2206  # --tests is a deliberate word-split list of codes
PYNFS_ARGS+=($TESTS)

log "Running pynfs ${MINOR_VERSION} against ${SERVER}${EXPORT_PATH}"
log "  tests:   ${TESTS}"
log "  results: ${RESULTS_DIR}"

if $VERBOSE; then
    "$PYNFS_BIN" "${PYNFS_ARGS[@]}" 2>&1 | tee "$LOG_FILE"
else
    "$PYNFS_BIN" "${PYNFS_ARGS[@]}" > "$LOG_FILE" 2>&1
fi

# pynfs exits non-zero when tests fail; that is expected input to the grader,
# not a harness error, so the verdict comes from parse-results.sh alone.
cp /tmp/dittofs-posix-server.log "${RESULTS_DIR}/dittofs.log" 2>/dev/null || true

echo ""
echo -e "${BOLD}=== Grading against $(basename "$KNOWN_FAILURES") ===${NC}"
"${SCRIPT_DIR}/parse-results.sh" "$LOG_FILE" "$KNOWN_FAILURES" "$RESULTS_DIR"
GRADE=$?

echo ""
log "Log:     ${LOG_FILE}"
if [[ "$GRADE" -eq 0 ]]; then
    log "PASSED (no new failures)"
else
    log_warn "FAILED (${GRADE} new failure(s))"
fi
exit "$GRADE"
