#!/bin/bash

# DittoFS E2E Test Runner
# This script orchestrates running e2e tests with optional Localstack for S3 tests

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default options
RUN_S3_TESTS=false
VERBOSE=false
SPECIFIC_TEST=""
KEEP_LOCALSTACK=false

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --s3)
            RUN_S3_TESTS=true
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
        --keep-localstack)
            KEEP_LOCALSTACK=true
            shift
            ;;
        --help|-h)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --s3                 Run S3 tests (requires Localstack)"
            echo "  --verbose, -v        Enable verbose test output"
            echo "  --test, -t NAME      Run specific test (e.g., TestCreateFolder)"
            echo "  --keep-localstack    Keep Localstack running after tests"
            echo "  --help, -h           Show this help message"
            echo ""
            echo "Examples:"
            echo "  $0                              # Run standard tests (no S3)"
            echo "  $0 --s3                         # Run all tests including S3"
            echo "  $0 --test TestCreateFile_1MB   # Run specific test"
            echo "  $0 --s3 --verbose               # Run with verbose output"
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

# Function to check if Localstack is healthy
wait_for_localstack() {
    echo -e "${YELLOW}Waiting for Localstack to be ready...${NC}"

    local max_attempts=30
    local attempt=1

    while [ $attempt -le $max_attempts ]; do
        if curl -s http://localhost:4566/_localstack/health > /dev/null 2>&1; then
            echo -e "${GREEN}Localstack is ready!${NC}"
            return 0
        fi

        echo -n "."
        sleep 1
        attempt=$((attempt + 1))
    done

    echo -e "${RED}Localstack failed to start${NC}"
    return 1
}

# Function to stop Localstack
stop_localstack() {
    if [ "$KEEP_LOCALSTACK" = false ]; then
        echo -e "${YELLOW}Stopping Localstack...${NC}"
        docker-compose down -v
        echo -e "${GREEN}Localstack stopped${NC}"
    else
        echo -e "${YELLOW}Keeping Localstack running (use 'docker-compose down' to stop)${NC}"
    fi
}

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

    if [ "$RUN_S3_TESTS" = true ]; then
        stop_localstack
    fi

    exit $exit_code
}

# Set up cleanup trap
trap cleanup EXIT

# Build test flags
TEST_FLAGS="-timeout 30m"

if [ "$VERBOSE" = true ]; then
    TEST_FLAGS="$TEST_FLAGS -v"
fi

if [ -n "$SPECIFIC_TEST" ]; then
    TEST_FLAGS="$TEST_FLAGS -run $SPECIFIC_TEST"
fi

# Start Localstack if running S3 tests
if [ "$RUN_S3_TESTS" = true ]; then
    echo -e "${YELLOW}Starting Localstack for S3 tests...${NC}"

    # Check if docker-compose is available
    if ! command -v docker-compose &> /dev/null; then
        echo -e "${RED}docker-compose not found. Please install docker-compose.${NC}"
        exit 1
    fi

    # Start Localstack
    docker-compose up -d

    # Wait for Localstack to be ready
    if ! wait_for_localstack; then
        echo -e "${RED}Failed to start Localstack${NC}"
        docker-compose logs
        exit 1
    fi

    # Set environment variable for tests
    export LOCALSTACK_ENDPOINT="http://localhost:4566"

    echo ""
fi

# Run tests
echo -e "${BLUE}Running tests...${NC}"
echo -e "${YELLOW}Test flags: $TEST_FLAGS${NC}"
echo ""

if [ "$RUN_S3_TESTS" = true ]; then
    echo -e "${YELLOW}Running ALL tests (including S3)${NC}"
    echo ""
    go test $TEST_FLAGS ./...
else
    echo -e "${YELLOW}Running standard tests (excluding S3)${NC}"
    echo -e "${YELLOW}Use --s3 flag to include S3 tests${NC}"
    echo ""
    # Run tests but skip S3-specific ones
    go test $TEST_FLAGS ./... -skip "S3"
fi

echo ""
