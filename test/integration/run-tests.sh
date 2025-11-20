#!/bin/bash

# Integration Test Runner for DittoFS
#
# This script orchestrates running integration tests that require external services
# (Localstack for S3, etc.). It handles starting/stopping services and running tests.
#
# Usage:
#   ./run-tests.sh              # Run all integration tests
#   ./run-tests.sh s3           # Run only S3 tests
#   ./run-tests.sh --no-docker  # Skip Docker, assume services are running
#   ./run-tests.sh --cleanup    # Clean up Docker resources and exit

set -e  # Exit on error

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/docker-compose.yml"
LOCALSTACK_ENDPOINT="http://localhost:4566"
DOCKER_COMPOSE="docker compose"

# Check if docker-compose (with hyphen) is available
if ! command -v docker &> /dev/null || ! docker compose version &> /dev/null 2>&1; then
    if command -v docker-compose &> /dev/null; then
        DOCKER_COMPOSE="docker-compose"
    else
        echo -e "${RED}Error: Neither 'docker compose' nor 'docker-compose' is available${NC}"
        echo "Please install Docker Compose: https://docs.docker.com/compose/install/"
        exit 1
    fi
fi

# Default options
USE_DOCKER=true
CLEANUP_ONLY=false
TEST_FILTER=""
VERBOSE=false

# ============================================================================
# Helper Functions
# ============================================================================

print_header() {
    echo -e "\n${BLUE}===================================================================${NC}"
    echo -e "${BLUE}$1${NC}"
    echo -e "${BLUE}===================================================================${NC}\n"
}

print_success() {
    echo -e "${GREEN}✓ $1${NC}"
}

print_error() {
    echo -e "${RED}✗ $1${NC}"
}

print_warning() {
    echo -e "${YELLOW}⚠ $1${NC}"
}

print_info() {
    echo -e "${BLUE}ℹ $1${NC}"
}

# ============================================================================
# Parse Arguments
# ============================================================================

while [[ $# -gt 0 ]]; do
    case $1 in
        --no-docker)
            USE_DOCKER=false
            shift
            ;;
        --cleanup)
            CLEANUP_ONLY=true
            shift
            ;;
        --verbose|-v)
            VERBOSE=true
            shift
            ;;
        --help|-h)
            echo "Usage: $0 [OPTIONS] [TEST_FILTER]"
            echo ""
            echo "Options:"
            echo "  --no-docker       Skip Docker operations (assume services running)"
            echo "  --cleanup         Clean up Docker resources and exit"
            echo "  --verbose, -v     Enable verbose output"
            echo "  --help, -h        Show this help message"
            echo ""
            echo "Test Filters:"
            echo "  s3                Run only S3 integration tests"
            echo "  badger            Run only Badger integration tests"
            echo "  (empty)           Run all integration tests"
            echo ""
            echo "Examples:"
            echo "  $0                      # Run all integration tests"
            echo "  $0 s3                   # Run only S3 tests"
            echo "  $0 --no-docker          # Run tests without managing Docker"
            echo "  $0 --cleanup            # Clean up Docker resources"
            exit 0
            ;;
        s3|badger)
            TEST_FILTER=$1
            shift
            ;;
        *)
            print_error "Unknown option: $1"
            echo "Use --help for usage information"
            exit 1
            ;;
    esac
done

# ============================================================================
# Cleanup Function
# ============================================================================

cleanup_docker() {
    print_header "Cleaning up Docker resources"

    cd "$SCRIPT_DIR"

    if $DOCKER_COMPOSE -f "$COMPOSE_FILE" ps -q 2>/dev/null | grep -q .; then
        print_info "Stopping Docker services..."
        $DOCKER_COMPOSE -f "$COMPOSE_FILE" down -v --remove-orphans
        print_success "Docker services stopped and removed"
    else
        print_info "No running Docker services found"
    fi
}

# Handle cleanup-only mode
if $CLEANUP_ONLY; then
    cleanup_docker
    exit 0
fi

# Setup cleanup trap
if $USE_DOCKER; then
    trap cleanup_docker EXIT
fi

# ============================================================================
# Start Docker Services
# ============================================================================

# Skip Docker for badger tests (BadgerDB is embedded, no external services needed)
if [ "$TEST_FILTER" = "badger" ]; then
    print_info "BadgerDB tests don't require Docker (embedded database)"
elif $USE_DOCKER; then
    print_header "Starting Docker services"

    cd "$SCRIPT_DIR"

    # Start services
    print_info "Starting Localstack..."
    $DOCKER_COMPOSE -f "$COMPOSE_FILE" up -d

    # Wait for Localstack to be healthy
    print_info "Waiting for Localstack to be ready..."
    RETRIES=30
    RETRY_COUNT=0

    while [ $RETRY_COUNT -lt $RETRIES ]; do
        if curl -sf "$LOCALSTACK_ENDPOINT/_localstack/health" > /dev/null 2>&1; then
            print_success "Localstack is ready!"
            break
        fi

        RETRY_COUNT=$((RETRY_COUNT + 1))
        if [ $RETRY_COUNT -eq $RETRIES ]; then
            print_error "Localstack failed to start within timeout"
            exit 1
        fi

        echo -n "."
        sleep 1
    done
    echo ""

    # Show Localstack health
    if $VERBOSE; then
        print_info "Localstack health status:"
        curl -s "$LOCALSTACK_ENDPOINT/_localstack/health" | jq '.' 2>/dev/null || \
            curl -s "$LOCALSTACK_ENDPOINT/_localstack/health"
    fi
else
    print_warning "Skipping Docker setup (--no-docker flag)"
    print_info "Ensure required services are running:"
    print_info "  - Localstack: $LOCALSTACK_ENDPOINT"
fi

# ============================================================================
# Run Integration Tests
# ============================================================================

print_header "Running Integration Tests"

cd "$PROJECT_ROOT"

# Set environment variables for tests
export LOCALSTACK_ENDPOINT="$LOCALSTACK_ENDPOINT"

# Build test command
TEST_CMD="go test -tags=integration -v"

if $VERBOSE; then
    TEST_CMD="$TEST_CMD -count=1"  # Disable test cache for verbose mode
fi

# Add timeout
TEST_CMD="$TEST_CMD -timeout=10m"

# Determine which tests to run
if [ -z "$TEST_FILTER" ]; then
    # Run all integration tests
    print_info "Running all integration tests..."
    TEST_PACKAGES="./test/integration/..."
elif [ "$TEST_FILTER" = "s3" ]; then
    print_info "Running S3 integration tests..."
    TEST_PACKAGES="./test/integration/s3"
elif [ "$TEST_FILTER" = "badger" ]; then
    print_info "Running Badger integration tests..."
    TEST_PACKAGES="./test/integration/badger"
else
    print_error "Unknown test filter: $TEST_FILTER"
    exit 1
fi

# Run tests
print_info "Command: $TEST_CMD $TEST_PACKAGES"
echo ""

if $TEST_CMD $TEST_PACKAGES; then
    print_success "All integration tests passed!"
    EXIT_CODE=0
else
    print_error "Integration tests failed!"
    EXIT_CODE=1
fi

# ============================================================================
# Summary
# ============================================================================

print_header "Test Summary"

if [ $EXIT_CODE -eq 0 ]; then
    print_success "Integration tests completed successfully"
else
    print_error "Integration tests failed (exit code: $EXIT_CODE)"
fi

exit $EXIT_CODE
