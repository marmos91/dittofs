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

VALID_PROFILES=("memory" "memory-fs" "badger-fs")

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
  --timeout SECONDS   Kill smbtorture after SECONDS (default: 1200 = 20 min)
                      Also settable via SMBTORTURE_TIMEOUT env var
  --keep              Leave containers running after tests
  --dry-run           Show configuration and exit
  --verbose           Enable verbose output
  --help              Show this help

Profiles:
  memory        Memory metadata + memory payload (fastest)
  memory-fs     Memory metadata + filesystem payload
  badger-fs     BadgerDB metadata + filesystem payload

Examples:
  $(basename "$0")                              # Full smb2 suite with memory
  $(basename "$0") --filter smb2.connect        # Run only smb2.connect tests
  $(basename "$0") --profile badger-fs          # Test with persistent backend
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
    echo ""
    echo -e "${BOLD}=== smbtorture Test Configuration ===${NC}"
    echo ""
    echo "  Profile:     ${PROFILE}"
    echo "  Filter:      ${FILTER:-smb2 (full suite)}"
    echo "  Timeout:     ${TIMEOUT}s"
    echo "  Keep:        ${KEEP}"
    echo "  Verbose:     ${VERBOSE}"
    echo ""
    echo "  Results dir:  ${RESULTS_DIR}"
    echo ""
    echo "  Docker image: quay.io/samba.org/samba-toolbox:v0.8"
    echo "  Target:       //localhost/smbbasic"
    echo "  Auth:         wpts-admin / TestPassword01!"
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

# Bootstrap DittoFS (same as WPTS -- creates shares, users, SMB adapter)
log_step "Bootstrapping DittoFS (profile: ${PROFILE})..."
docker compose exec \
    -e DFSCTL="/app/dfsctl" \
    -e API_URL="http://localhost:8080" \
    -e ADMIN_PASSWORD="${admin_password}" \
    -e TEST_PASSWORD="TestPassword01!" \
    -e PROFILE="${PROFILE}" \
    -e SMB_PORT="445" \
    dittofs sh /app/bootstrap.sh

# Build smbtorture command override (when --filter is provided)
smbtorture_cmd=()
if [[ -n "$FILTER" ]]; then
    smbtorture_cmd=(
        "//localhost/smbbasic"
        "-U" "wpts-admin%TestPassword01!"
        "--option=client min protocol=SMB2_02"
        "--option=client max protocol=SMB3"
        "$FILTER"
    )
fi

# Run smbtorture
# When smbtorture_cmd is empty, docker compose uses the default command from
# docker-compose.yml. When populated (--filter), it overrides the entrypoint args.
# The ${arr[@]+"${arr[@]}"} pattern safely expands empty arrays under set -u.
#
# timeout(1) kills the process after TIMEOUT seconds. Some smbtorture tests
# (e.g. hold-sharemode) block indefinitely waiting for SIGINT, so a timeout
# prevents CI from hanging until the job-level timeout cancels the run.
log_step "Running smbtorture (filter: ${FILTER:-smb2}, timeout: ${TIMEOUT}s)..."
smbtorture_exit=0

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

${TIMEOUT_CMD:+$TIMEOUT_CMD --signal=TERM --kill-after=30 "$TIMEOUT"} \
    env PROFILE="$PROFILE" docker compose run --rm smbtorture \
    ${smbtorture_cmd[@]+"${smbtorture_cmd[@]}"} \
    2>&1 | tee "${RESULTS_DIR}/smbtorture-output.txt" || smbtorture_exit=$?

if [ "$smbtorture_exit" -eq 124 ]; then
    log_warn "smbtorture timed out after ${TIMEOUT}s (some hold-tests block indefinitely)"
elif [ "$smbtorture_exit" -ne 0 ]; then
    log_warn "smbtorture exited with code ${smbtorture_exit} (expected -- failures are classified)"
fi

# Collect DittoFS logs
log_step "Collecting DittoFS logs..."
docker compose logs dittofs > "${RESULTS_DIR}/dittofs.log" 2>&1 || true

# Parse results
log_step "Parsing results..."
parse_exit=0
VERBOSE="$VERBOSE" "${SCRIPT_DIR}/parse-results.sh" \
    "${RESULTS_DIR}/smbtorture-output.txt" \
    "${SCRIPT_DIR}/KNOWN_FAILURES.md" \
    "${RESULTS_DIR}" \
    || parse_exit=$?

echo ""
echo -e "${BOLD}Results directory:${NC} ${RESULTS_DIR}"
echo ""

exit "$parse_exit"
