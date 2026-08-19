#!/usr/bin/env bash
# Generates the CA, server and client certificates the PyKMIP container and
# the Go client authenticate each other with. Output goes to $1 (default
# ./certs). Everything is throwaway test material with a 2-day lifetime.
#
# The keys are ECDSA P-256 rather than RSA on purpose. PyKMIP's "TLS1.2"
# authentication suite advertises exactly two AEAD cipher suites,
# ECDHE-ECDSA-AES128-GCM-SHA256 and ECDHE-ECDSA-AES256-GCM-SHA384; every
# RSA-authenticated entry in that suite is CBC-SHA256, which Go removed
# from its default TLS 1.2 cipher list. With an RSA server certificate the
# two sides share no cipher suite and the handshake fails outright.
set -euo pipefail

OUT="${1:-$(dirname "$0")/certs}"
mkdir -p "$OUT"

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 \
  -keyout "$OUT/ca-key.pem" -out "$OUT/ca-cert.pem" \
  -subj "/CN=dittofs-kmip-test-ca" >/dev/null 2>&1

gen() {
  local name="$1" cn="$2" eku="$3" san="$4"
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "$OUT/$name-key.pem" -out "$OUT/$name.csr" \
    -subj "/CN=$cn" >/dev/null 2>&1
  openssl x509 -req -in "$OUT/$name.csr" -days 2 \
    -CA "$OUT/ca-cert.pem" -CAkey "$OUT/ca-key.pem" -CAcreateserial \
    -out "$OUT/$name-cert.pem" \
    -extfile <(printf 'extendedKeyUsage=%s\nsubjectAltName=%s\n' "$eku" "$san") \
    >/dev/null 2>&1
  rm -f "$OUT/$name.csr"
}

gen server dittofs-kmip-server serverAuth "DNS:localhost,IP:127.0.0.1"
# PyKMIP derives the client identity from the certificate common name and
# makes that identity the owner of every object it creates, so the
# provisioning script and the Go tests have to present this same
# certificate or the Get is denied by the default access policy.
gen client dittofs-kmip-client clientAuth "DNS:dittofs-kmip-client"

chmod 0600 "$OUT"/*-key.pem
echo "$OUT"
