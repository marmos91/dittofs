#!/bin/bash
# Run Kerberos integration tests with a real MIT KDC in Docker.
#
# Usage:
#   ./run.sh              # Run all tests
#   ./run.sh -v           # Verbose output
#   ./run.sh -run krb5i   # Run specific subtest
set -euo pipefail

cd "$(dirname "$0")/../../.."

echo "=== Kerberos Integration Test ==="
echo "Requires: Docker"
echo ""

exec go test -tags=kerberos -timeout 5m ./test/integration/kerberos/ "$@"
