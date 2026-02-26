#!/usr/bin/env bash
# =============================================================================
# DittoFS E2E Test Runner
# =============================================================================
#
# Runs E2E tests with optional features: S3 (Localstack), stress tests,
# coverage profiling, NFS version selection, and specific test patterns.
#
# Usage:
#   sudo ./run-e2e.sh [options]
#
# Options:
#   --s3                 Start Localstack for S3 tests (stop after tests)
#   --keep-localstack    Keep Localstack running after tests (for repeated runs)
#   --test PATTERN       Run specific test pattern (-run PATTERN)
#   --verbose            Enable verbose test output (-v)
#   --coverage           Generate coverage profile (-coverprofile=coverage-e2e.out)
#   --stress             Include stress tests (-tags='e2e,stress')
#   --nfs-version VER    Set DITTOFS_E2E_NFS_VERSION env var (3, 4, 4.0)
#   --timeout DURATION   Set test timeout (default: 30m)
#   --race               Enable race detector (-race)
#   --portmap            Run only portmapper tests (TestPortmapper)
#   --help               Show this help message
#
# Examples:
#   sudo ./run-e2e.sh                                  # Run all E2E tests
#   sudo ./run-e2e.sh --verbose                        # Run with verbose output
#   sudo ./run-e2e.sh --test TestNFSv4BasicOperations  # Run specific test
#   sudo ./run-e2e.sh --coverage                       # Generate coverage profile
#   sudo ./run-e2e.sh --stress --verbose               # Include stress tests
#   sudo ./run-e2e.sh --s3                             # Include S3 tests
#   sudo ./run-e2e.sh --portmap                        # Run portmapper tests only
#   sudo ./run-e2e.sh --nfs-version 4                  # Set NFS version for tests

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# =============================================================================
# Default options
# =============================================================================
USE_S3=false
KEEP_LOCALSTACK=false
TEST_PATTERN=""
VERBOSE=false
COVERAGE=false
STRESS=false
NFS_VERSION=""
TIMEOUT="30m"
RACE=false
PORTMAP=false

# =============================================================================
# Colors for output
# =============================================================================
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }
log_step()  { echo -e "${BLUE}[STEP]${NC} $*"; }

usage() {
    cat <<'USAGE'
Usage: sudo ./run-e2e.sh [options]

Options:
  --s3                 Start Localstack for S3 tests (stop after tests)
  --keep-localstack    Keep Localstack running after tests (for repeated runs)
  --test PATTERN       Run specific test pattern (-run PATTERN)
  --verbose            Enable verbose test output (-v)
  --coverage           Generate coverage profile (-coverprofile=coverage-e2e.out)
  --stress             Include stress tests (-tags='e2e,stress')
  --nfs-version VER    Set DITTOFS_E2E_NFS_VERSION env var (3, 4, 4.0)
  --timeout DURATION   Set test timeout (default: 30m)
  --race               Enable race detector (-race)
  --portmap            Run only portmapper tests (TestPortmapper)
  --help               Show this help message

Examples:
  sudo ./run-e2e.sh                                  # Run all E2E tests
  sudo ./run-e2e.sh --verbose                        # Run with verbose output
  sudo ./run-e2e.sh --test TestNFSv4BasicOperations  # Run specific test
  sudo ./run-e2e.sh --s3                             # Include S3 tests
  sudo ./run-e2e.sh --portmap                        # Run portmapper tests only
  sudo ./run-e2e.sh --nfs-version 4                  # Set NFS version for tests
USAGE
}

# =============================================================================
# Parse arguments
# =============================================================================
while [[ $# -gt 0 ]]; do
    case $1 in
        --s3)
            USE_S3=true
            shift
            ;;
        --keep-localstack)
            KEEP_LOCALSTACK=true
            shift
            ;;
        --test)
            TEST_PATTERN="${2:-}"
            shift 2
            ;;
        --test=*)
            TEST_PATTERN="${1#*=}"
            shift
            ;;
        --verbose)
            VERBOSE=true
            shift
            ;;
        --coverage)
            COVERAGE=true
            shift
            ;;
        --stress)
            STRESS=true
            shift
            ;;
        --nfs-version)
            NFS_VERSION="${2:-}"
            shift 2
            ;;
        --nfs-version=*)
            NFS_VERSION="${1#*=}"
            shift
            ;;
        --timeout)
            TIMEOUT="${2:-}"
            shift 2
            ;;
        --timeout=*)
            TIMEOUT="${1#*=}"
            shift
            ;;
        --race)
            RACE=true
            shift
            ;;
        --portmap)
            PORTMAP=true
            shift
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        *)
            log_error "Unknown option: $1"
            echo "Use --help for usage information."
            exit 1
            ;;
    esac
done

# =============================================================================
# Check prerequisites
# =============================================================================
if [[ $EUID -ne 0 ]]; then
    log_error "This script must be run as root (sudo)"
    log_error "NFS mount tests require root privileges."
    exit 1
fi

if ! command -v go &>/dev/null; then
    log_error "'go' command not found. Please install Go or activate your environment."
    exit 1
fi

# =============================================================================
# Build go test command
# =============================================================================
BUILD_TAGS="e2e"
if [[ "$STRESS" == "true" ]]; then
    BUILD_TAGS="e2e,stress"
fi

GO_TEST_ARGS=(
    "go" "test"
    "-tags=${BUILD_TAGS}"
    "-timeout" "$TIMEOUT"
)

if [[ "$VERBOSE" == "true" ]]; then
    GO_TEST_ARGS+=("-v")
fi

if [[ "$RACE" == "true" ]]; then
    GO_TEST_ARGS+=("-race")
fi

if [[ "$COVERAGE" == "true" ]]; then
    COVERAGE_FILE="${REPO_ROOT}/coverage-e2e.out"
    GO_TEST_ARGS+=("-coverprofile=${COVERAGE_FILE}")
    GO_TEST_ARGS+=("-coverpkg=./pkg/...,./internal/...,./cmd/...")
fi

if [[ "$PORTMAP" == "true" ]]; then
    TEST_PATTERN="TestPortmapper"
fi

if [[ -n "$TEST_PATTERN" ]]; then
    GO_TEST_ARGS+=("-run" "$TEST_PATTERN")
fi

GO_TEST_ARGS+=("./test/e2e/...")

# =============================================================================
# Set environment variables
# =============================================================================
if [[ -n "$NFS_VERSION" ]]; then
    export DITTOFS_E2E_NFS_VERSION="$NFS_VERSION"
fi

# =============================================================================
# Localstack management (for S3 tests)
# =============================================================================
LOCALSTACK_CONTAINER=""

start_localstack() {
    log_step "Starting Localstack for S3 tests..."

    # Check if Localstack is already running
    if docker ps --format '{{.Names}}' 2>/dev/null | grep -q "dittofs-localstack"; then
        log_info "Localstack already running (dittofs-localstack)"
        LOCALSTACK_CONTAINER="dittofs-localstack"
        return
    fi

    # Start Localstack
    docker run -d \
        --name dittofs-localstack \
        -p 4566:4566 \
        -e SERVICES=s3 \
        -e DEFAULT_REGION=us-east-1 \
        localstack/localstack:4.13.1

    LOCALSTACK_CONTAINER="dittofs-localstack"

    # Wait for Localstack to be ready
    log_info "Waiting for Localstack to be ready..."
    local max_attempts=30
    local attempt=1
    while [[ $attempt -le $max_attempts ]]; do
        if curl -s http://localhost:4566/_localstack/health 2>/dev/null | grep -q '"s3": "available"'; then
            log_info "Localstack is ready"
            return
        fi
        sleep 1
        ((attempt++))
    done

    log_error "Localstack failed to start within $max_attempts seconds"
    exit 1
}

stop_localstack() {
    if [[ -n "$LOCALSTACK_CONTAINER" ]] && [[ "$KEEP_LOCALSTACK" == "false" ]]; then
        log_step "Stopping Localstack..."
        docker rm -f "$LOCALSTACK_CONTAINER" 2>/dev/null || true
    elif [[ "$KEEP_LOCALSTACK" == "true" ]]; then
        log_info "Keeping Localstack running (--keep-localstack)"
    fi
}

# =============================================================================
# Run tests
# =============================================================================
log_info "DittoFS E2E Test Runner"
log_info "======================"
log_info "Build tags:    ${BUILD_TAGS}"
log_info "Timeout:       ${TIMEOUT}"
log_info "Verbose:       ${VERBOSE}"
log_info "Coverage:      ${COVERAGE}"
log_info "Stress:        ${STRESS}"
log_info "S3:            ${USE_S3}"
log_info "Race:          ${RACE}"
log_info "Portmap:       ${PORTMAP}"
if [[ -n "$NFS_VERSION" ]]; then
    log_info "NFS version:   ${NFS_VERSION}"
fi
if [[ -n "$TEST_PATTERN" ]]; then
    log_info "Test pattern:  ${TEST_PATTERN}"
fi
echo ""

# Start Localstack if needed
if [[ "$USE_S3" == "true" ]]; then
    start_localstack
fi

# Run the tests
log_step "Running: ${GO_TEST_ARGS[*]}"
echo ""

START_TIME=$(date +%s)
TEST_EXIT_CODE=0
"${GO_TEST_ARGS[@]}" || TEST_EXIT_CODE=$?
END_TIME=$(date +%s)

DURATION=$((END_TIME - START_TIME))

# =============================================================================
# Print summary
# =============================================================================
echo ""
echo "============================================="
if [[ $TEST_EXIT_CODE -eq 0 ]]; then
    log_info "E2E tests PASSED (${DURATION}s)"
else
    log_error "E2E tests FAILED (exit code: ${TEST_EXIT_CODE}, ${DURATION}s)"
fi
echo "============================================="

if [[ "$COVERAGE" == "true" ]] && [[ -f "${COVERAGE_FILE:-}" ]]; then
    log_info "Coverage profile: ${COVERAGE_FILE}"
    # Show brief coverage summary
    go tool cover -func="${COVERAGE_FILE}" 2>/dev/null | tail -1 || true
fi

# Stop Localstack if needed
if [[ "$USE_S3" == "true" ]]; then
    stop_localstack
fi

exit $TEST_EXIT_CODE
