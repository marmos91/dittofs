#!/usr/bin/env bash
# Run POSIX compliance tests for DittoFS
#
# Usage (from nix shell):
#   cd /tmp/dittofs-test
#   sudo env PATH="$PATH" /path/to/run-posix.sh [test_pattern]
#
# Examples:
#   sudo env PATH="$PATH" ./run-posix.sh                # Run all tests
#   sudo env PATH="$PATH" ./run-posix.sh chmod          # Run chmod tests
#   sudo env PATH="$PATH" ./run-posix.sh 'chmod/*.t'    # Run specific test pattern

set -euo pipefail

MOUNT_POINT="${DITTOFS_MOUNT:-/tmp/dittofs-test}"

# Check if running as root
if [[ $EUID -ne 0 ]]; then
    echo "Error: This script must be run as root (sudo)"
    exit 1
fi

# Check if mount point exists and is mounted
if ! mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
    echo "Error: $MOUNT_POINT is not mounted"
    echo ""
    echo "Start DittoFS and mount with:"
    echo "  ./dfs start"
    echo "  sudo mkdir -p $MOUNT_POINT"
    echo "  sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,nolock localhost:/export $MOUNT_POINT"
    exit 1
fi

# Helper function to check if a pjdfstest binary has tests directory
has_tests_dir() {
    local bin="$1"
    local tests_dir
    tests_dir="$(dirname "$bin")/../share/pjdfstest/tests"
    [[ -d "$tests_dir" ]]
}

# Find pjdfstest binary that has tests directory
PJDFSTEST_BIN=""
TESTS_DIR=""

# First, try pjdfstest from PATH (works when run from nix develop shell)
if command -v pjdfstest &>/dev/null; then
    candidate="$(which pjdfstest)"
    if has_tests_dir "$candidate"; then
        PJDFSTEST_BIN="$candidate"
    fi
fi

# If not found or doesn't have tests, search nix store for one that does
if [[ -z "$PJDFSTEST_BIN" ]]; then
    for candidate in $(find /nix/store -path "*/bin/pjdfstest" -type f -executable 2>/dev/null); do
        if has_tests_dir "$candidate"; then
            PJDFSTEST_BIN="$candidate"
            break
        fi
    done
fi

if [[ -z "$PJDFSTEST_BIN" ]]; then
    echo "Error: pjdfstest not found (or no version with tests directory)"
    echo "Enter nix development shell: nix develop"
    exit 1
fi

TESTS_DIR="$(dirname "$PJDFSTEST_BIN")/../share/pjdfstest/tests"

# pjdfstest's misc.sh searches for the binary by traversing up the directory tree.
# Since nix store is read-only and the binary is in bin/ (not a parent of tests/),
# we need to create a working directory with a symlink to the binary.
WORK_DIR=$(mktemp -d)
trap "rm -rf '$WORK_DIR'" EXIT

# Create symlink to pjdfstest binary at the working directory root
ln -s "$PJDFSTEST_BIN" "$WORK_DIR/pjdfstest"

# Copy the tests directory structure (symlinks to actual test files)
# The conf file is inside tests/, so cp -rs includes it
cp -rs "$TESTS_DIR" "$WORK_DIR/tests"

cd "$MOUNT_POINT"

echo "Running POSIX compliance tests..."
echo "Mount point: $MOUNT_POINT"
echo "Tests directory: $WORK_DIR/tests"
echo "pjdfstest binary: $PJDFSTEST_BIN"
echo ""

if [[ $# -gt 0 ]]; then
    # Run specific test pattern
    exec prove -rv "$WORK_DIR/tests/$1"
else
    # Run all tests
    exec prove -rv "$WORK_DIR/tests"
fi
