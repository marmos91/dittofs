#!/usr/bin/env bash
# smbtorture test runner for DittoFS SMB conformance
# GPL compliance: smbtorture runs inside Docker container only
#
# Usage:
#   ./run.sh                                  # Run full smb2 suite with memory profile
#   ./run.sh --profile badger-fs              # Run with specific profile
#   ./run.sh --filter smb2.connect            # Run specific sub-test
#   ./run.sh --keep                           # Leave containers running for debugging
#   ./run.sh --dry-run                        # Show configuration and exit
#   ./run.sh --verbose                        # Enable verbose output

set -euo pipefail

# --------------------------------------------------------------------------
# Constants
# --------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFORMANCE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

VALID_PROFILES=("memory" "memory-fs" "badger-fs" "memory-kerberos")

# --------------------------------------------------------------------------
# Colors
# --------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[SMBTORTURE]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[SMBTORTURE]${NC} $*"; }
log_error() { echo -e "${RED}[SMBTORTURE]${NC} $*"; }
log_step()  { echo -e "${CYAN}[SMBTORTURE]${NC} ${BOLD}$*${NC}"; }

# wait_until CMD MAX_ATTEMPTS LABEL
wait_until() {
    local cmd="$1" max="$2" label="$3"
    local attempt=1
    while [ "$attempt" -le "$max" ]; do
        if eval "$cmd" >/dev/null 2>&1; then
            log_info "${label} is ready"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_error "${label} not ready after ${max}s"
    return 1
}

# --------------------------------------------------------------------------
# Defaults
# --------------------------------------------------------------------------
PROFILE="${PROFILE:-memory}"
FILTER=""
KEEP=false
DRY_RUN=false
VERBOSE=false
KERBEROS=false
TIMEOUT="${SMBTORTURE_TIMEOUT:-1200}"  # Default 20 minutes

# --------------------------------------------------------------------------
# Parse arguments
# --------------------------------------------------------------------------
usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Run smbtorture SMB2 test suite against DittoFS SMB adapter.
GPL compliance: smbtorture executes inside a Docker container only.

Options:
  --profile PROFILE   Storage profile (default: memory)
                      Valid: ${VALID_PROFILES[*]}
  --filter FILTER     smbtorture test filter (e.g., smb2.connect, smb2.lock)
                      Default: full smb2 suite
  --kerberos          Run smbtorture with Kerberos (SPNEGO) authentication.
                      Requires KDC infrastructure. Sets --use-kerberos=required
                      and configures Kerberos realm for the test environment.
                      Also settable via SMBTORTURE_AUTH=kerberos env var.
  --timeout SECONDS   Kill smbtorture after SECONDS (default: 1200 = 20 min)
                      Also settable via SMBTORTURE_TIMEOUT env var
  --keep              Leave containers running after tests
  --dry-run           Show configuration and exit
  --verbose           Enable verbose output
  --help              Show this help

Profiles:
  memory           Memory metadata + memory payload (fastest)
  memory-fs        Memory metadata + memory payload (legacy name, same as memory)
  badger-fs        BadgerDB metadata + memory payload (legacy name)
  memory-kerberos  Memory profile with Kerberos auth enabled (auto-selected by --kerberos)

Examples:
  $(basename "$0")                              # Full smb2 suite with memory
  $(basename "$0") --filter smb2.connect        # Run only smb2.connect tests
  $(basename "$0") --profile badger-fs          # Test with persistent backend
  $(basename "$0") --kerberos --filter smb2.session  # Kerberos session tests
  $(basename "$0") --keep --verbose             # Debug a failure
  $(basename "$0") --timeout 600               # 10 minute timeout
EOF
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --profile)
            PROFILE="${2:?--profile requires a value}"
            shift 2
            ;;
        --filter)
            FILTER="${2:?--filter requires a value}"
            shift 2
            ;;
        --keep)
            KEEP=true
            shift
            ;;
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        --timeout)
            TIMEOUT="${2:?--timeout requires a value}"
            shift 2
            ;;
        --kerberos)
            KERBEROS=true
            shift
            ;;
        --verbose)
            VERBOSE=true
            shift
            ;;
        --help|-h)
            usage
            ;;
        *)
            log_error "Unknown option: $1"
            echo "Run with --help for usage."
            exit 1
            ;;
    esac
done

# SMBTORTURE_AUTH=kerberos is treated as equivalent to --kerberos so that
# callers driving the runner via env vars get the full Kerberos setup (KDC
# service, memory-kerberos profile, bootstrap identity mapping), not just
# the smbtorture argument switch.
if [[ "${SMBTORTURE_AUTH:-}" == "kerberos" ]]; then
    KERBEROS=true
fi

# When --kerberos is set, force the Kerberos-enabled config profile.
# memory-kerberos wires up the keytab path and service principal that the
# self-contained kdc container provisions at startup. Any other profile is
# silently overridden (with a warning for non-memory variants).
if $KERBEROS && [[ "$PROFILE" != "memory-kerberos" ]]; then
    if [[ "$PROFILE" != "memory" && "$PROFILE" != "memory-fs" ]]; then
        log_warn "Profile ${PROFILE} does not include Kerberos config; forcing memory-kerberos"
    fi
    PROFILE="memory-kerberos"
fi

# --------------------------------------------------------------------------
# Validate inputs
# --------------------------------------------------------------------------
validate_profile() {
    for p in "${VALID_PROFILES[@]}"; do
        [[ "$p" == "$PROFILE" ]] && return 0
    done
    log_error "Invalid profile: ${PROFILE}"
    echo "Valid profiles: ${VALID_PROFILES[*]}"
    exit 1
}

validate_profile

# --------------------------------------------------------------------------
# Results directory
# --------------------------------------------------------------------------
RESULTS_DIR="${CONFORMANCE_DIR}/results/smbtorture-$(date +%Y-%m-%d_%H%M%S)"

# --------------------------------------------------------------------------
# Dry-run
# --------------------------------------------------------------------------
if $DRY_RUN; then
    if $KERBEROS; then
        dry_target="//dittofs/smbbasic"
        dry_auth="wpts-admin@DITTOFS.TEST (Kerberos, SPNEGO)"
    else
        dry_target="//localhost/smbbasic"
        dry_auth="wpts-admin / TestPassword01!"
    fi

    echo ""
    echo -e "${BOLD}=== smbtorture Test Configuration ===${NC}"
    echo ""
    echo "  Profile:     ${PROFILE}"
    echo "  Filter:      ${FILTER:-smb2 (full suite)}"
    echo "  Kerberos:    ${KERBEROS}"
    echo "  Timeout:     ${TIMEOUT}s"
    echo "  Keep:        ${KEEP}"
    echo "  Verbose:     ${VERBOSE}"
    echo ""
    echo "  Results dir:  ${RESULTS_DIR}"
    echo ""
    echo "  Docker image: quay.io/samba.org/samba-toolbox:v0.8"
    echo "  Target:       ${dry_target}"
    echo "  Auth:         ${dry_auth}"
    echo ""
    exit 0
fi

# --------------------------------------------------------------------------
# Cleanup handler
# --------------------------------------------------------------------------
cleanup() {
    local exit_code=$?

    if ! $KEEP; then
        log_step "Cleaning up containers..."
        cd "$CONFORMANCE_DIR"
        docker compose down -v 2>/dev/null || true
    else
        log_warn "Containers left running (--keep). Clean up with: cd ${CONFORMANCE_DIR} && docker compose down -v"
    fi

    return $exit_code
}
trap cleanup EXIT

# --------------------------------------------------------------------------
# Main execution
# --------------------------------------------------------------------------
echo ""
echo -e "${BOLD}=== smbtorture Test Runner ===${NC}"
echo ""
log_info "Profile: ${PROFILE}"
log_info "Filter:  ${FILTER:-smb2 (full suite)}"
if [[ "$(uname -m)" == "arm64" ]]; then
    log_warn "ARM64 detected -- smbtorture image will run under Rosetta/QEMU emulation (linux/amd64)"
fi
echo ""

cd "$CONFORMANCE_DIR"

mkdir -p "$RESULTS_DIR"

# Kerberos mode: activate the "kerberos" compose profile (enables the kdc
# and smbtorture-kerberos services) and start the self-contained KDC first
# so it has time to create the realm and export /keytabs/dittofs.keytab.
# DittoFS mounts the same volume read-only and loads the keytab on startup.
#
# COMPOSE_PROFILES is exported via env (rather than --profile flags) to
# sidestep macOS bash 3.2's "empty array + set -u" expansion quirk.
if $KERBEROS; then
    export COMPOSE_PROFILES="kerberos"

    log_step "Building KDC Docker image..."
    docker compose build kdc

    log_step "Starting KDC..."
    docker compose up -d kdc
    # klist parses the full keytab; only succeeds once kadmin has finished
    # writing and flushing the file, avoiding a partial-read race.
    wait_until "docker compose exec kdc klist -k /keytabs/dittofs.keytab > /dev/null 2>&1" 60 "KDC keytab"
fi

# Build and start DittoFS
log_step "Building DittoFS Docker image..."
PROFILE="$PROFILE" docker compose build dittofs

log_step "Starting DittoFS (profile: ${PROFILE})..."
PROFILE="$PROFILE" docker compose up -d dittofs

wait_until "docker compose exec dittofs wget -q --spider http://localhost:8080/health/ready" 60 "DittoFS"

# Extract auto-generated admin password
log_step "Extracting admin password from DittoFS logs..."
admin_password=""
admin_password=$(docker compose logs dittofs 2>/dev/null | grep -o 'password: [^ ]*' | head -1 | awk '{print $2}' || echo "")
if [[ -z "$admin_password" ]]; then
    log_error "Could not extract admin password from DittoFS logs"
    exit 1
fi
if $VERBOSE; then
    log_info "Admin password extracted"
fi

# Bootstrap DittoFS (same as WPTS -- creates shares, users, SMB adapter).
# The KERBEROS env var tells bootstrap.sh to configure the SMB adapter with
# Kerberos auth and seed the identity mapping for wpts-admin@DITTOFS.TEST.
log_step "Bootstrapping DittoFS (profile: ${PROFILE})..."
docker compose exec \
    -e DFSCTL="/app/dfsctl" \
    -e API_URL="http://localhost:8080" \
    -e ADMIN_PASSWORD="${admin_password}" \
    -e TEST_PASSWORD="TestPassword01!" \
    -e PROFILE="${PROFILE}" \
    -e SMB_PORT="445" \
    -e KERBEROS="$($KERBEROS && echo 1 || echo 0)" \
    dittofs sh /app/bootstrap.sh

# --------------------------------------------------------------------------
# smbtorture execution
# --------------------------------------------------------------------------

# Use gtimeout on macOS (GNU coreutils), timeout on Linux
TIMEOUT_CMD="timeout"
if ! command -v timeout >/dev/null 2>&1; then
    if command -v gtimeout >/dev/null 2>&1; then
        TIMEOUT_CMD="gtimeout"
    else
        log_warn "Neither timeout nor gtimeout found; running without timeout guard"
        TIMEOUT_CMD=""
    fi
fi

# Common smbtorture arguments
# NOTE: "netbios name=localhost" is required because smbtorture uses its
# NetBIOS name for secondary IPC$ connections. Without this, the default
# name ("smbtorture" - the binary name) doesn't resolve in Docker and
# secondary connections fail with NT_STATUS_OBJECT_NAME_NOT_FOUND.
#
# SMBTORTURE_HOST holds the bare host (no share), so run_smbtorture can swap
# the share per suite (e.g. acls_non_canonical → /smbnoncanon while default
# acls runs against /smbbasic).
if $KERBEROS; then
    # Kerberos mode: target DittoFS by its docker service name "dittofs" so
    # the client requests a ticket for cifs/dittofs@DITTOFS.TEST (which is
    # the SPN the kdc service exports to /keytabs/dittofs.keytab). The
    # smbtorture-kerberos compose service mounts the shared keytab volume
    # and sets KRB5_CONFIG=/keytabs/krb5.conf so gssapi finds the KDC.
    SMBTORTURE_SERVICE="smbtorture-kerberos"
    SMBTORTURE_HOST="//dittofs"
    SMBTORTURE_AUTH_ARGS=(
        "-U" "wpts-admin@DITTOFS.TEST%TestPassword01!"
        "--use-kerberos=required"
        "--realm=DITTOFS.TEST"
        "--option=netbios name=localhost"
        "--option=client min protocol=SMB2_02"
        "--option=client max protocol=SMB3"
        "--option=torture:smbd=false"
        # Reserved server-side ACL xattr name surfaced to smbtorture
        # smb2.ea.acl_xattr. The server rejects EA writes targeting this name
        # with STATUS_ACCESS_DENIED (set_info.go::reservedACLXattrName).
        "--option=torture:acl_xattr_name=security.NTACL"
    )
    log_info "Kerberos mode: targeting ${SMBTORTURE_HOST}/<share> with SPNEGO/Kerberos"
else
    SMBTORTURE_SERVICE="smbtorture"
    SMBTORTURE_HOST="//localhost"
    SMBTORTURE_AUTH_ARGS=(
        "-U" "wpts-admin%TestPassword01!"
        "--option=netbios name=localhost"
        "--option=client min protocol=SMB2_02"
        "--option=client max protocol=SMB3"
        "--option=torture:smbd=false"
        # Reserved server-side ACL xattr name surfaced to smbtorture
        # smb2.ea.acl_xattr. The server rejects EA writes targeting this name
        # with STATUS_ACCESS_DENIED (set_info.go::reservedACLXattrName).
        "--option=torture:acl_xattr_name=security.NTACL"
    )
fi

# Default share for most suites. smb2.acls_non_canonical overrides via the
# 4th run_smbtorture argument because that suite requires
# `acl flag inherited canonicalization = no` (Samba extension) on the share,
# whereas the default smb2.acls suite requires Windows-default canonicalization.
SMBTORTURE_DEFAULT_SHARE="${SMBTORTURE_DEFAULT_SHARE:-smbbasic}"

# run_smbtorture FILTER [PER_TEST_TIMEOUT] [SUITE_PREFIX] [SHARE]
# Runs smbtorture with the given filter, appending output to results file.
# When SUITE_PREFIX is set, test/success/failure/error lines in the output
# get the prefix prepended so that KNOWN_FAILURES.md wildcards match correctly.
# (Running smb2.oplock reports "test: batch1" but known failures expect
#  "oplock.batch1", so we fix up the output.)
# When SHARE is set, that share replaces SMBTORTURE_DEFAULT_SHARE in the
# target UNC. Used by smb2.acls_non_canonical to target /smbnoncanon.
run_smbtorture() {
    local filter="$1"
    local per_timeout="${2:-$TIMEOUT}"
    local suite_prefix="${3:-}"
    local share="${4:-$SMBTORTURE_DEFAULT_SHARE}"
    local target="${SMBTORTURE_HOST}/${share}"

    local rc=0
    if [[ -n "$suite_prefix" ]]; then
        ${TIMEOUT_CMD:+$TIMEOUT_CMD --signal=TERM --kill-after=30 "$per_timeout"} \
            env PROFILE="$PROFILE" docker compose run --rm "$SMBTORTURE_SERVICE" \
            "$target" "${SMBTORTURE_AUTH_ARGS[@]}" "$filter" \
            2>&1 | sed -E "s/^(test|success|failure|error|skip): /\1: ${suite_prefix}./" \
            | tee -a "${RESULTS_DIR}/smbtorture-output.txt" || rc=${PIPESTATUS[0]}
    else
        ${TIMEOUT_CMD:+$TIMEOUT_CMD --signal=TERM --kill-after=30 "$per_timeout"} \
            env PROFILE="$PROFILE" docker compose run --rm "$SMBTORTURE_SERVICE" \
            "$target" "${SMBTORTURE_AUTH_ARGS[@]}" "$filter" \
            2>&1 | tee -a "${RESULTS_DIR}/smbtorture-output.txt" || rc=${PIPESTATUS[0]}
    fi
    # Classify the exit code (see _smbtorture_exit handling at end of file):
    #   124            -> the per-suite timeout fired (a hang we DO want to fail on)
    #   125            -> docker run/daemon error (image pull 502, OOM-killed
    #                     container, etc.) — a real infrastructure failure
    #   128+N (>=129)  -> the smbtorture CLIENT process was killed by signal N
    #                     (e.g. 134=SIGABRT from smb_panic, 139=SIGSEGV). This is
    #                     a smbtorture bug, NOT a DittoFS or infra fault, so it
    #                     must not by itself fail the job — parse-results.sh is
    #                     the source of truth for protocol outcomes. We log it and
    #                     let the run continue / be graded on parsed results.
    # 126/127 are also docker-side (permission / command-not-found) and are
    # treated as infrastructure like 125.
    if [[ $rc -ge 129 ]]; then
        log_warn "smbtorture client crashed (exit code $rc, signal $((rc - 128))) for filter: $filter — client-side bug, not failing the job on this alone"
    elif [[ $rc -ge 124 ]]; then
        log_warn "smbtorture infrastructure failure (exit code $rc) for filter: $filter"
    fi
    return $rc
}

# reset_share SHARE
# Returns a share to clean state between sub-suites by delegating to
# bootstrap.sh's reset-share subcommand inside the DittoFS container, which
# deletes and recreates the share via the admin REST API. Refs #568.
#
# smbtorture sub-suites run sequentially against the same shares; a suite that
# fails mid-test (notably the ACL suites) can leave restrictive DACLs on files,
# non-empty directories, and dangling opens/lease records. The next suite then
# sees that leftover state and a previously-passing test fails — a rotating,
# scheduling-dependent spurious "new failure". Delete+recreate clears file/dir
# state AND drops server-side opens + lease records bound to the share, without
# disturbing users, identity mappings, or the SMB adapter config.
#
# bootstrap.sh rotates the admin password to TEST_PASSWORD on first login, so
# reset-share authenticates with TEST_PASSWORD (not the original log-scraped
# admin_password). Failures are logged but non-fatal: a reset hiccup must not
# abort the run — at worst it reintroduces the flake for one suite rather than
# corrupting results.
reset_share() {
    local share="$1"
    if ! docker compose exec \
        -e DFSCTL="/app/dfsctl" \
        -e API_URL="http://localhost:8080" \
        -e TEST_PASSWORD="TestPassword01!" \
        -e PROFILE="${PROFILE}" \
        dittofs sh /app/bootstrap.sh reset-share "/${share}" >/dev/null 2>&1; then
        log_warn "  Share reset failed for /${share}; continuing"
    fi
}

# _smbtorture_exit  : last non-zero run_smbtorture exit (kept for context/logging)
# _smbtorture_infra : highest exit code that represents a REAL docker/infra
#                     failure (125-127: daemon error, image-pull 502,
#                     OOM-killed container, permission/command-not-found). This
#                     preserves the prior `>=125` job-failure threshold while
#                     EXCLUDING smbtorture client process crashes (>=129, killed
#                     by signal) — those are upstream client bugs and must not
#                     red the job; parse-results.sh grades the actual protocol
#                     outcomes from whatever output was produced. A per-suite
#                     timeout (124) is also left non-fatal, matching the prior
#                     behaviour (partial output is still graded).
_smbtorture_exit=0
_smbtorture_infra=0

# record_rc RC: fold a run_smbtorture exit code into the trackers.
record_rc() {
    local rc="$1"
    [[ $rc -ne 0 ]] && _smbtorture_exit=$rc
    if [[ $rc -ge 125 && $rc -le 127 && $rc -gt $_smbtorture_infra ]]; then
        _smbtorture_infra=$rc
    fi
}

if [[ -n "$FILTER" ]]; then
    # Single filter mode: run only the specified filter
    log_step "Running smbtorture (filter: ${FILTER}, timeout: ${TIMEOUT}s)..."
    run_smbtorture "$FILTER" || record_rc $?
else
    # Full suite mode: run sub-suites individually to avoid hold-oplock and
    # hold-sharemode tests which block indefinitely (they are interactive
    # diagnostic tools, not real conformance tests).
    #
    # Each sub-suite's output is prefixed with the suite name so that
    # KNOWN_FAILURES.md wildcard patterns (e.g. smb2.oplock.*) match.
    log_step "Running smbtorture sub-suites (skipping hold tests, timeout: ${TIMEOUT}s)..."

    # Standalone tests (no prefix needed - these are top-level tests)
    STANDALONE_TESTS=(
        smb2.connect smb2.setinfo smb2.stream-inherit-perms
        smb2.set-sparse-ioctl smb2.zero-data-ioctl smb2.ioctl-on-stream
        smb2.dosmode smb2.async_dosmode smb2.maxfid
        smb2.check-sharemode smb2.openattr smb2.winattr smb2.winattr2
        smb2.sdread smb2.secleak smb2.session-id smb2.tcon smb2.mkdir
    )
    for test in "${STANDALONE_TESTS[@]}"; do
        # Reset the default share before each suite so leftover state from a
        # prior failed suite (restrictive DACLs, non-empty dirs, dangling
        # opens/leases) can't fail a previously-passing test. Refs #568.
        reset_share "$SMBTORTURE_DEFAULT_SHARE"
        log_info "  Running: ${test}"
        run_smbtorture "$test" 60 || record_rc $?
    done

    # Sub-suites with prefix for test name fixup.
    # "smb2.oplock" runs tests like "batch1" which need "oplock." prefix
    # to become "oplock.batch1" matching "smb2.oplock.*" known failures.
    # Format: "suite:prefix" or "suite:prefix:share" triples. The optional
    # third field overrides SMBTORTURE_DEFAULT_SHARE for that suite — used
    # by smb2.acls_non_canonical which needs the Samba extension
    # `acl flag inherited canonicalization = no` enabled on the share.
    SUITES=(
        "smb2.acls:acls"
        "smb2.acls_non_canonical:acls_non_canonical:smbnoncanon"
        "smb2.aio_delay:aio_delay"
        "smb2.bench:bench"
        "smb2.change_notify_disabled:change_notify_disabled:change_notify_disabled"
        "smb2.charset:charset"
        "smb2.compound:compound"
        "smb2.compound_async:compound_async"
        "smb2.compound_find:compound_find"
        "smb2.create:create"
        "smb2.create_no_streams:create_no_streams:create_no_streams"
        "smb2.credits:credits"
        "smb2.delete-on-close-perms:delete-on-close-perms"
        "smb2.deny:deny"
        "smb2.dir:dir"
        # smb2.dirlease is run per-subtest with smb2.dirlease.oplocks
        # skipped: smbtorture 4.22.6 client SIGSEGVs inside that subtest and
        # aborts the rest of the dirlease suite, hiding pass/fail for the 17
        # other subtests. Tracked in #633 — drop this workaround once the
        # smbtorture client crash is fixed upstream (or we upgrade past it).
        "smb2.dirlease.v2_request:dirlease"
        "smb2.dirlease.v2_request_parent:dirlease"
        "smb2.dirlease.leases:dirlease"
        "smb2.dirlease.overwrite:dirlease"
        "smb2.dirlease.rename:dirlease"
        "smb2.dirlease.rename_dst_parent:dirlease"
        "smb2.dirlease.hardlink:dirlease"
        "smb2.dirlease.setatime:dirlease"
        "smb2.dirlease.setbtime:dirlease"
        "smb2.dirlease.setctime:dirlease"
        "smb2.dirlease.setmtime:dirlease"
        "smb2.dirlease.setdos:dirlease"
        "smb2.dirlease.seteof:dirlease"
        "smb2.dirlease.unlink_same_initial_and_close:dirlease"
        "smb2.dirlease.unlink_same_set_and_close:dirlease"
        "smb2.dirlease.unlink_different_initial_and_close:dirlease"
        "smb2.dirlease.unlink_different_set_and_close:dirlease"
        "smb2.durable-open:durable-open"
        "smb2.durable-open-disconnect:durable-open-disconnect"
        # Refs #739: run durable-v2-open against the CA share /smbpersistent so
        # the persistent-open-{oplock,lease} subtests take their CA path
        # (SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY → durable==true &&
        # persistent==true for every row). The non-persistent durable subtests
        # use their own CA-independent tables (they gate on SCALEOUT, which the
        # share does not advertise), so the CA share does not affect them.
        "smb2.durable-v2-open:durable-v2-open:smbpersistent"
        "smb2.durable-v2-delay:durable-v2-delay"
        "smb2.durable-v2-regressions:durable-v2-regressions"
        "smb2.ea:ea"
        "smb2.fileid:fileid"
        "smb2.getinfo:getinfo"
        "smb2.ioctl:ioctl"
        "smb2.kernel-oplocks:kernel-oplocks"
        "smb2.lease:lease"
        "smb2.lock:lock"
        "smb2.maximum_allowed:maximum_allowed"
        "smb2.multichannel:multichannel"
        "smb2.name-mangling:name-mangling"
        "smb2.notify:notify"
        "smb2.notify-inotify:notify-inotify"
        "smb2.oplock:oplock"
        "smb2.read:read"
        "smb2.rename:rename"
        "smb2.replay:replay"
        "smb2.rw:rw"
        "smb2.samba3misc:samba3misc"
        # smb2.scan is run per-subtest with the smb2.scan.scan opcode-fuzzer
        # skipped: it walks every SMB2 command id, and at opcode 12
        # (SMB2_OPLOCK_BREAK) the smbtorture 4.22.6 *client* aborts inside its
        # OWN signing code — smb2_signing_calc_signature asserts
        # "opcode[12] msg_id == 0" and smb_panic()s
        # (libcli/smb/smb2_signing.c:576). The backtrace is entirely in the
        # client (smb2_signing_sign_pdu → smb2cli_req_compound_submit); DittoFS
        # is not in it and correctly returns NT_STATUS_INVALID_PARAMETER for the
        # bogus opcodes it does receive (it is pure Go and cannot SIGSEGV here).
        # The client abort surfaces as a docker exit code >=129 (128+signal),
        # which the infrastructure-failure guard below historically turned into
        # a red job — the recurring "exit 139 / smb2.scan" memory-profile flake.
        # (The guard now ignores client-crash codes, but skipping the test is
        # still preferable so the suite produces real results.) The other three
        # scan subtests (getinfo/setinfo/find) do not crash and are kept. Same
        # workaround shape as smb2.dirlease.oplocks (#633). Drop smb2.scan.scan
        # from the skip list once the smbtorture client crash is fixed upstream
        # (or we upgrade past 4.22.6).
        "smb2.scan.getinfo:scan"
        "smb2.scan.setinfo:scan"
        "smb2.scan.find:scan"
        "smb2.session:session"
        "smb2.session-require-signing:session-require-signing"
        "smb2.sharemode:sharemode"
        "smb2.streams:streams"
        "smb2.timestamp_resolution:timestamp_resolution"
        "smb2.timestamps:timestamps"
        "smb2.twrp:twrp"
    )
    for entry in "${SUITES[@]}"; do
        # Split "suite:prefix" or "suite:prefix:share" using IFS=:.
        IFS=':' read -r suite prefix share <<< "$entry"
        # Reset the share this suite targets before running it, so leftover
        # state from a prior failed suite can't fail a previously-passing test.
        # Refs #568. Multiple dirlease sub-suites share /smbbasic, so this also
        # isolates them from one another.
        reset_share "${share:-$SMBTORTURE_DEFAULT_SHARE}"
        if [[ -n "${share:-}" ]]; then
            log_info "  Running: ${suite} (share: ${share})"
        else
            log_info "  Running: ${suite}"
        fi
        run_smbtorture "$suite" 120 "$prefix" "${share:-}" || record_rc $?
    done

    # NOTE: Skipped interactive hold tests:
    #   smb2.hold-oplock    - waits 5 min for oplock events (no real test)
    #   smb2.hold-sharemode - blocks indefinitely waiting for SIGINT
    log_warn "Skipped: smb2.hold-oplock, smb2.hold-sharemode (interactive hold tests)"
fi

# Collect DittoFS logs
log_step "Collecting DittoFS logs..."
docker compose logs dittofs > "${RESULTS_DIR}/dittofs.log" 2>&1 || true

# Parse results
log_step "Parsing results..."
parse_exit=0
KNOWN_FAILURES_PATH="${SCRIPT_DIR}/KNOWN_FAILURES.md"
if $KERBEROS && [[ -f "${SCRIPT_DIR}/KNOWN_FAILURES_KERBEROS.md" ]]; then
    KNOWN_FAILURES_PATH="${SCRIPT_DIR}/KNOWN_FAILURES_KERBEROS.md"
fi
VERBOSE="$VERBOSE" "${SCRIPT_DIR}/parse-results.sh" \
    "${RESULTS_DIR}/smbtorture-output.txt" \
    "${KNOWN_FAILURES_PATH}" \
    "${RESULTS_DIR}" \
    || parse_exit=$?

echo ""
echo -e "${BOLD}Results directory:${NC} ${RESULTS_DIR}"
echo ""

# Fail on genuine docker/infra errors (exit 125-127) even if parse-results
# found no new test failures — same threshold as before this change.
# smbtorture *client* process crashes (exit >=129, killed by signal) are
# intentionally NOT job-failing on their own: they are upstream client bugs
# (see the smb2.scan.scan note above), and the run is graded on the protocol
# outcomes parse-results.sh extracted from whatever output was produced before
# the crash. record_rc only records 125-127 into _smbtorture_infra.
if [[ $_smbtorture_infra -ge 125 ]]; then
    log_error "smbtorture had infrastructure failures (exit code $_smbtorture_infra)"
    exit "$_smbtorture_infra"
fi

exit "$parse_exit"
