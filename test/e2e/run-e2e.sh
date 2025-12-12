#!/bin/bash

# DittoFS E2E Test Runner
# This script orchestrates running e2e tests
# External services (PostgreSQL, Localstack) are managed by testcontainers

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default options
RUN_S3_TESTS=true
RUN_POSTGRES_TESTS=true
VERBOSE=false
SPECIFIC_TEST=""

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --s3)
            RUN_S3_TESTS=true
            shift
            ;;
        --no-s3)
            RUN_S3_TESTS=false
            shift
            ;;
        --postgres)
            RUN_POSTGRES_TESTS=true
            shift
            ;;
        --no-postgres)
            RUN_POSTGRES_TESTS=false
            shift
            ;;
        --local)
            RUN_S3_TESTS=false
            RUN_POSTGRES_TESTS=false
            shift
            ;;
        --verbose|-v)
            VERBOSE=true
            shift
            ;;
        --test|-t)
            SPECIFIC_TEST="$2"
            shift 2
            ;;
        --help|-h)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --s3                 Run S3 tests (default, requires Docker)"
            echo "  --no-s3              Skip S3 tests"
            echo "  --postgres           Run PostgreSQL tests (default, requires Docker)"
            echo "  --no-postgres        Skip PostgreSQL tests"
            echo "  --local              Run only local tests (no Docker required)"
            echo "  --verbose, -v        Enable verbose test output"
            echo "  --test, -t NAME      Run specific test (e.g., TestCreateFolder)"
            echo "  --help, -h           Show this help message"
            echo ""
            echo "External Services:"
            echo "  PostgreSQL and Localstack are managed automatically via testcontainers."
            echo "  Docker must be running for S3 and PostgreSQL tests."
            echo ""
            echo "  To use external services instead of testcontainers, set:"
            echo "    POSTGRES_HOST, POSTGRES_PORT, POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DATABASE"
            echo "    LOCALSTACK_ENDPOINT"
            echo ""
            echo "Examples:"
            echo "  $0                              # Run all tests (default)"
            echo "  $0 --local                      # Run only local tests (no Docker)"
            echo "  $0 --no-s3                      # Skip S3 tests"
            echo "  $0 --test TestCreateFile_1MB    # Run specific test"
            echo "  $0 --verbose                    # Run with verbose output"
            exit 0
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            echo "Use --help for usage information"
            exit 1
            ;;
    esac
done

# Get script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}DittoFS E2E Test Runner${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""

# Check Docker availability if needed
if [ "$RUN_S3_TESTS" = true ] || [ "$RUN_POSTGRES_TESTS" = true ]; then
    if ! command -v docker &> /dev/null; then
        echo -e "${RED}Docker not found. Please install Docker.${NC}"
        echo -e "${YELLOW}Use --local to run tests without Docker${NC}"
        exit 1
    fi

    if ! docker info &> /dev/null; then
        echo -e "${RED}Docker daemon is not running. Please start Docker.${NC}"
        echo -e "${YELLOW}Use --local to run tests without Docker${NC}"
        exit 1
    fi

    echo -e "${GREEN}Docker is available${NC}"
fi

# Cleanup function
cleanup() {
    local exit_code=$?
    echo ""

    if [ $exit_code -eq 0 ]; then
        echo -e "${GREEN}========================================${NC}"
        echo -e "${GREEN}Tests completed successfully!${NC}"
        echo -e "${GREEN}========================================${NC}"
    else
        echo -e "${RED}========================================${NC}"
        echo -e "${RED}Tests failed!${NC}"
        echo -e "${RED}========================================${NC}"
    fi

    exit $exit_code
}

# Set up cleanup trap
trap cleanup EXIT

# Build test flags
TEST_FLAGS="-tags=e2e -timeout 30m"

if [ "$VERBOSE" = true ]; then
    TEST_FLAGS="$TEST_FLAGS -v"
fi

if [ -n "$SPECIFIC_TEST" ]; then
    TEST_FLAGS="$TEST_FLAGS -run $SPECIFIC_TEST"
fi

# Build skip pattern
SKIP_PATTERNS=""

if [ "$RUN_S3_TESTS" = false ]; then
    SKIP_PATTERNS="${SKIP_PATTERNS}S3|"
fi

if [ "$RUN_POSTGRES_TESTS" = false ]; then
    SKIP_PATTERNS="${SKIP_PATTERNS}postgres|Postgres|"
fi

# Remove trailing |
SKIP_PATTERNS="${SKIP_PATTERNS%|}"

# Run tests
echo -e "${BLUE}Running tests...${NC}"
echo -e "${YELLOW}Test flags: $TEST_FLAGS${NC}"

if [ "$RUN_S3_TESTS" = true ]; then
    echo -e "${GREEN}S3 tests: enabled (testcontainers)${NC}"
else
    echo -e "${YELLOW}S3 tests: disabled${NC}"
fi

if [ "$RUN_POSTGRES_TESTS" = true ]; then
    echo -e "${GREEN}PostgreSQL tests: enabled (testcontainers)${NC}"
else
    echo -e "${YELLOW}PostgreSQL tests: disabled${NC}"
fi

echo ""

if [ -n "$SKIP_PATTERNS" ]; then
    echo -e "${YELLOW}Skipping patterns: $SKIP_PATTERNS${NC}"
    go test $TEST_FLAGS ./... -skip "$SKIP_PATTERNS"
else
    echo -e "${GREEN}Running ALL tests${NC}"
    go test $TEST_FLAGS ./...
fi

echo ""
