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
    echo "  ./dittofs start"
    echo "  sudo mkdir -p $MOUNT_POINT"
    echo "  sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,nolock localhost:/export $MOUNT_POINT"
    exit 1
fi

# Find pjdfstest binary to locate tests directory
if command -v pjdfstest &>/dev/null; then
    PJDFSTEST_BIN="$(which pjdfstest)"
else
    # Look in nix store
    PJDFSTEST_BIN=$(find /nix/store -name "pjdfstest" -type f -executable 2>/dev/null | head -1)
    if [[ -z "$PJDFSTEST_BIN" ]]; then
        echo "Error: pjdfstest not found"
        echo "Enter nix development shell: nix develop"
        exit 1
    fi
fi

# Find tests directory relative to pjdfstest binary
TESTS_DIR="$(dirname "$PJDFSTEST_BIN")/../share/pjdfstest/tests"
if [[ ! -d "$TESTS_DIR" ]]; then
    echo "Error: pjdfstest tests not found at $TESTS_DIR"
    exit 1
fi

cd "$MOUNT_POINT"

echo "Running POSIX compliance tests..."
echo "Mount point: $MOUNT_POINT"
echo "Tests directory: $TESTS_DIR"
echo ""

if [[ $# -gt 0 ]]; then
    # Run specific test pattern
    exec prove -rv "$TESTS_DIR/$1"
else
    # Run all tests
    exec prove -rv "$TESTS_DIR"
fi
