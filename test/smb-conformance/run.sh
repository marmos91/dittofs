#!/usr/bin/env bash
# SMB Conformance Test Runner
# Orchestrates WPTS FileServer BVT tests against DittoFS
#
# Usage:
#   ./run.sh                                     # Run BVT tests with memory profile
#   ./run.sh --profile badger-fs                 # Run with specific profile
#   ./run.sh --mode local                        # Run DittoFS natively, WPTS in Docker
#   ./run.sh --filter "TestCategory=BVT"         # Custom test filter
#   ./run.sh --category BVT                      # Shorthand for --filter
#   ./run.sh --keep                              # Leave containers running for debugging
#   ./run.sh --dry-run                           # Show config and exit
#   ./run.sh --verbose                           # Enable verbose output

set -euo pipefail

# --------------------------------------------------------------------------
# Constants
# --------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

VALID_PROFILES=("memory" "memory-fs" "badger-fs" "badger-s3" "postgres-s3")

# --------------------------------------------------------------------------
# Colors
# --------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[RUN]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[RUN]${NC} $*"; }
log_error() { echo -e "${RED}[RUN]${NC} $*"; }
log_step()  { echo -e "${CYAN}[RUN]${NC} ${BOLD}$*${NC}"; }

# find_newest_file DIR PATTERN
# Finds the most recently modified file matching PATTERN in DIR.
# Compatible with both BSD (macOS) and GNU (Linux) stat.
find_newest_file() {
    local dir="$1" pattern="$2"
    find "$dir" -name "$pattern" -type f 2>/dev/null \
        | while IFS= read -r f; do
            local mtime
            mtime=$(stat --format='%Y' "$f" 2>/dev/null || stat -f '%m' "$f" 2>/dev/null)
            echo "$mtime $f"
        done \
        | sort -rn | head -1 | cut -d' ' -f2-
}

# wait_until CMD MAX_ATTEMPTS LABEL
# Retries CMD every second up to MAX_ATTEMPTS times. Logs LABEL on success/failure.
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

# collect_and_parse_results RESULTS_DIR [--skip-docker-logs]
# Finds the TRX file, copies it to RESULTS_DIR, and runs parse-results.sh.
# Pass --skip-docker-logs in local mode where DittoFS logs are already captured.
collect_and_parse_results() {
    local results_dir="$1"
    local skip_docker_logs="${2:-}"

    log_step "Collecting results..."
    if [[ "$skip_docker_logs" != "--skip-docker-logs" ]]; then
        docker compose logs dittofs > "${results_dir}/dittofs.log" 2>&1 || true
    fi

    local trx_file=""
    trx_file=$(find_newest_file "${SCRIPT_DIR}/ptfconfig-generated" "*.trx")

    if [[ -z "$trx_file" ]]; then
        log_error "No TRX file found in ptfconfig-generated/"
        log_error "WPTS may not have produced output. Check dittofs.log:"
        log_error "  ${results_dir}/dittofs.log"
        exit 1
    fi

    cp "$trx_file" "${results_dir}/"
    local trx_basename
    trx_basename="$(basename "$trx_file")"

    log_info "TRX results: ${results_dir}/${trx_basename}"
    log_info "DittoFS logs: ${results_dir}/dittofs.log"

    log_step "Parsing results..."
    local parse_exit=0
    VERBOSE="$VERBOSE" "${SCRIPT_DIR}/parse-results.sh" "${results_dir}/${trx_basename}" "${SCRIPT_DIR}/KNOWN_FAILURES.md" || parse_exit=$?

    echo ""
    echo -e "${BOLD}Results directory:${NC} ${results_dir}"
    echo ""

    return "$parse_exit"
}

# --------------------------------------------------------------------------
# Defaults
# --------------------------------------------------------------------------
PROFILE="${PROFILE:-memory}"
MODE="compose"
FILTER="${WPTS_FILTER:-TestCategory=BVT}"
KEEP=false
DRY_RUN=false
VERBOSE=false

# --------------------------------------------------------------------------
# Parse arguments
# --------------------------------------------------------------------------
usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Orchestrate WPTS FileServer BVT tests against DittoFS SMB adapter.

Options:
  --profile PROFILE   Storage profile (default: memory)
                      Valid: ${VALID_PROFILES[*]}
  --mode MODE         Execution mode (default: compose)
                      Valid: compose, local
  --filter FILTER     WPTS test filter (default: TestCategory=BVT)
  --category CAT      Alias for --filter "TestCategory=CAT"
  --keep              Leave containers running after tests
  --dry-run           Show configuration and exit
  --verbose           Enable verbose output
  --help              Show this help

Profiles:
  memory        Memory metadata + memory payload (fastest)
  memory-fs     Memory metadata + filesystem payload
  badger-fs     BadgerDB metadata + filesystem payload
  badger-s3     BadgerDB metadata + S3 payload (requires Localstack)
  postgres-s3   PostgreSQL metadata + S3 payload (requires Localstack + PostgreSQL)

Examples:
  $(basename "$0")                              # Quick BVT test with memory
  $(basename "$0") --profile badger-s3          # Test with S3 backend
  $(basename "$0") --keep --verbose             # Debug a failure
  $(basename "$0") --category Model             # Run Model category tests
  $(basename "$0") --mode local --profile memory  # Native DittoFS + Docker WPTS
EOF
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --profile)
            PROFILE="${2:?--profile requires a value}"
            shift 2
            ;;
        --mode)
            MODE="${2:?--mode requires a value}"
            shift 2
            ;;
        --filter)
            FILTER="${2:?--filter requires a value}"
            shift 2
            ;;
        --category)
            FILTER="TestCategory=${2:?--category requires a value}"
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

validate_mode() {
    if [[ "$MODE" != "compose" && "$MODE" != "local" ]]; then
        log_error "Invalid mode: ${MODE} (must be 'compose' or 'local')"
        exit 1
    fi
}

validate_profile
validate_mode

# --------------------------------------------------------------------------
# ptfconfig template rendering
# --------------------------------------------------------------------------
export DITTOFS_HOST="localhost"
export SMB_PORT="445"
export CLIENT_IP="127.0.0.1"
export ADMIN_USER="wpts-admin"
export TEST_USER="nonadmin"
export TEST_PASSWORD="TestPassword01!"

render_ptfconfig() {
    log_step "Rendering ptfconfig templates..."

    local out_dir="${SCRIPT_DIR}/ptfconfig-generated"
    mkdir -p "$out_dir"

    # Clean stale TRX results from previous runs
    rm -rf "${out_dir}/TestResults"

    for tmpl in "${SCRIPT_DIR}"/ptfconfig/*.template; do
        [[ -f "$tmpl" ]] || continue
        local basename
        basename="$(basename "$tmpl" .template)"
        envsubst < "$tmpl" > "${out_dir}/${basename}"
        if $VERBOSE; then
            log_info "Rendered: ${basename}"
        fi
    done

    log_info "ptfconfig files written to ${out_dir}/"
}

# --------------------------------------------------------------------------
# Results directory
# --------------------------------------------------------------------------
RESULTS_DIR="${SCRIPT_DIR}/results/$(date +%Y-%m-%d_%H%M%S)"

# --------------------------------------------------------------------------
# Dry-run
# --------------------------------------------------------------------------
if $DRY_RUN; then
    render_ptfconfig

    echo ""
    echo -e "${BOLD}=== SMB Conformance Test Configuration ===${NC}"
    echo ""
    echo "  Profile:     ${PROFILE}"
    echo "  Mode:        ${MODE}"
    echo "  Filter:      ${FILTER}"
    echo "  Keep:        ${KEEP}"
    echo "  Verbose:     ${VERBOSE}"
    echo ""
    echo "  DITTOFS_HOST: ${DITTOFS_HOST}"
    echo "  SMB_PORT:     ${SMB_PORT}"
    echo "  CLIENT_IP:    ${CLIENT_IP}"
    echo "  ADMIN_USER:   ${ADMIN_USER}"
    echo "  TEST_USER:    ${TEST_USER}"
    echo ""
    echo "  Results dir:  ${RESULTS_DIR}"
    echo ""

    # Show compose profiles that would activate
    active_profiles=("test")
    case "$PROFILE" in
        *-s3)       active_profiles+=("s3") ;;
    esac
    case "$PROFILE" in
        postgres-*) active_profiles+=("postgres") ;;
    esac
    echo "  Compose profiles: ${active_profiles[*]}"
    echo ""
    echo "  ptfconfig templates:"
    for f in "${SCRIPT_DIR}"/ptfconfig-generated/*; do
        [[ -f "$f" ]] && echo "    - $(basename "$f")"
    done
    echo ""
    exit 0
fi

# --------------------------------------------------------------------------
# Cleanup handler
# --------------------------------------------------------------------------
cleanup() {
    local exit_code=$?

    if [[ "$MODE" == "compose" ]] && ! $KEEP; then
        log_step "Cleaning up containers..."
        cd "$SCRIPT_DIR"
        docker compose down -v 2>/dev/null || true
    elif [[ "$MODE" == "local" ]]; then
        # Stop local DittoFS process
        if [[ -n "${DITTOFS_PID:-}" ]] && kill -0 "$DITTOFS_PID" 2>/dev/null; then
            log_info "Stopping DittoFS (PID ${DITTOFS_PID})..."
            kill "$DITTOFS_PID" 2>/dev/null || true
            wait "$DITTOFS_PID" 2>/dev/null || true
        fi
        if ! $KEEP; then
            docker rm -f wpts-local 2>/dev/null || true
        fi
    fi

    if $KEEP; then
        log_warn "Containers left running (--keep). Clean up with: docker compose down -v"
    fi

    return $exit_code
}
trap cleanup EXIT

# --------------------------------------------------------------------------
# Compose mode
# --------------------------------------------------------------------------
run_compose() {
    cd "$SCRIPT_DIR"

    render_ptfconfig
    mkdir -p "$RESULTS_DIR"

    # Determine compose profiles
    local profiles=("--profile" "test")
    case "$PROFILE" in
        *-s3)       profiles+=("--profile" "s3") ;;
    esac
    case "$PROFILE" in
        postgres-*) profiles+=("--profile" "postgres") ;;
    esac

    # Build and start infrastructure
    log_step "Building DittoFS Docker image..."
    PROFILE="$PROFILE" docker compose build dittofs

    log_step "Starting infrastructure (profile: ${PROFILE})..."

    case "$PROFILE" in
        *-s3)
            PROFILE="$PROFILE" docker compose "${profiles[@]}" up -d localstack
            wait_until "docker compose exec localstack curl -sf http://localhost:4566/_localstack/health" 30 "Localstack"
            ;;
    esac
    case "$PROFILE" in
        postgres-*)
            PROFILE="$PROFILE" docker compose "${profiles[@]}" up -d postgres
            wait_until "docker compose exec postgres pg_isready -U dittofs -d dittofs_test" 30 "PostgreSQL"
            ;;
    esac

    PROFILE="$PROFILE" docker compose up -d dittofs
    wait_until "docker compose exec dittofs wget -q --spider http://localhost:8080/health/ready" 60 "DittoFS"

    # Extract auto-generated admin password from container logs
    log_step "Extracting admin password from DittoFS logs..."
    local admin_password=""
    admin_password=$(docker compose logs dittofs 2>/dev/null | grep -o 'password: [^ ]*' | head -1 | awk '{print $2}' || echo "")
    if [[ -z "$admin_password" ]]; then
        log_error "Could not extract admin password from DittoFS logs"
        return 1
    fi
    if $VERBOSE; then
        log_info "Admin password extracted"
    fi

    # Bootstrap DittoFS
    log_step "Bootstrapping DittoFS (profile: ${PROFILE})..."
    docker compose exec \
        -e DFSCTL="/app/dfsctl" \
        -e API_URL="http://localhost:8080" \
        -e ADMIN_PASSWORD="${admin_password}" \
        -e TEST_PASSWORD="${TEST_PASSWORD}" \
        -e PROFILE="${PROFILE}" \
        -e SMB_PORT="${SMB_PORT}" \
        dittofs sh /app/bootstrap.sh

    # Run WPTS
    log_step "Running WPTS tests (filter: ${FILTER})..."
    local wpts_exit=0
    WPTS_FILTER="$FILTER" PROFILE="$PROFILE" docker compose "${profiles[@]}" run --rm wpts || wpts_exit=$?

    if [ "$wpts_exit" -ne 0 ]; then
        log_warn "WPTS exited with code ${wpts_exit}"
    fi

    collect_and_parse_results "$RESULTS_DIR"
}

# --------------------------------------------------------------------------
# Local mode
# --------------------------------------------------------------------------
run_local() {
    render_ptfconfig
    mkdir -p "$RESULTS_DIR"

    # Build DittoFS natively
    log_step "Building DittoFS binaries..."
    cd "$REPO_ROOT"
    go build -o "${SCRIPT_DIR}/dfs" cmd/dfs/main.go
    go build -o "${SCRIPT_DIR}/dfsctl" cmd/dfsctl/main.go

    # On macOS, Docker Desktop needs host.docker.internal instead of localhost
    case "$(uname -s)" in
        Darwin) export DITTOFS_HOST="host.docker.internal" ;;
        *)      export DITTOFS_HOST="localhost" ;;
    esac
    render_ptfconfig

    # Start DittoFS in background
    log_step "Starting DittoFS (profile: ${PROFILE})..."
    "${SCRIPT_DIR}/dfs" start --foreground --config "${SCRIPT_DIR}/configs/${PROFILE}.yaml" \
        > "${RESULTS_DIR}/dittofs.log" 2>&1 &
    DITTOFS_PID=$!

    wait_until "curl -sf http://localhost:8080/health/ready" 60 "DittoFS"

    # Extract auto-generated admin password from server log
    log_step "Extracting admin password from DittoFS logs..."
    local admin_password=""
    admin_password=$(grep -o 'password: [^ ]*' "${RESULTS_DIR}/dittofs.log" 2>/dev/null | head -1 | awk '{print $2}' || echo "")
    if [[ -z "$admin_password" ]]; then
        log_error "Could not extract admin password from DittoFS log"
        return 1
    fi

    # Bootstrap
    log_step "Bootstrapping DittoFS (profile: ${PROFILE})..."
    DFSCTL="${SCRIPT_DIR}/dfsctl" \
    API_URL="http://localhost:8080" \
    ADMIN_PASSWORD="${admin_password}" \
    TEST_PASSWORD="${TEST_PASSWORD}" \
    PROFILE="${PROFILE}" \
    SMB_PORT="${SMB_PORT}" \
        bash "${SCRIPT_DIR}/bootstrap.sh"

    # Run WPTS via docker run
    log_step "Running WPTS tests (filter: ${FILTER})..."
    local docker_network=""
    if [[ "$(uname -s)" == "Linux" ]]; then
        docker_network="--network host"
    fi

    local wpts_exit=0
    # shellcheck disable=SC2086
    docker run --rm --name wpts-local \
        --platform linux/amd64 \
        ${docker_network} \
        -e Usage=RunTestCases \
        -e "Filter=${FILTER}" \
        -e DryRun=false \
        -e SutComputerName="${DITTOFS_HOST}" \
        -e "SutIPAddress=${DITTOFS_HOST}" \
        -e "DomainName=${DITTOFS_HOST}" \
        -e AdminUserName=wpts-admin \
        -e "PasswordForAllUsers=${TEST_PASSWORD}" \
        -v "${SCRIPT_DIR}/ptfconfig-generated:/data/fileserver" \
        mcr.microsoft.com/windowsprotocoltestsuites:fileserver-v8 || wpts_exit=$?

    if [ "$wpts_exit" -ne 0 ]; then
        log_warn "WPTS exited with code ${wpts_exit}"
    fi

    collect_and_parse_results "$RESULTS_DIR" --skip-docker-logs
}

# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------
echo ""
echo -e "${BOLD}=== SMB Conformance Test Runner ===${NC}"
echo ""
log_info "Profile: ${PROFILE}"
log_info "Mode:    ${MODE}"
log_info "Filter:  ${FILTER}"
if [[ "$(uname -m)" == "arm64" ]]; then
    log_warn "ARM64 detected — WPTS image will run under Rosetta/QEMU emulation (linux/amd64)"
fi
echo ""

case "$MODE" in
    compose) run_compose ;;
    local)   run_local   ;;
esac
