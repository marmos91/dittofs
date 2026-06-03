/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"
)

// selfSignedCAPEM returns a minimal self-signed CA certificate in PEM form for
// exercising the client's RootCAs wiring (the cert is never used to serve).
func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestNewDittoFSClientWithCA verifies the CA-trust wiring: an empty bundle keeps
// the default transport (system roots), a valid PEM CA installs a RootCAs pool
// WITHOUT disabling verification, and an invalid bundle is rejected rather than
// silently ignored.
func TestNewDittoFSClientWithCA(t *testing.T) {
	t.Run("empty CA falls back to default client", func(t *testing.T) {
		c, err := NewDittoFSClientWithCA("https://example.svc:8080", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Default client uses the default transport (nil) -> system roots.
		if c.httpClient.Transport != nil {
			t.Errorf("expected default (nil) transport for empty CA, got %T", c.httpClient.Transport)
		}
	})

	t.Run("valid CA installs RootCAs without skipping verification", func(t *testing.T) {
		// Generate a throwaway CA cert PEM via the api package test helper is in
		// another module; instead reuse a minimal self-signed PEM produced here.
		caPEM := selfSignedCAPEM(t)
		c, err := NewDittoFSClientWithCA("https://example.svc:8080", caPEM)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		tr, ok := c.httpClient.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("expected *http.Transport, got %T", c.httpClient.Transport)
		}
		if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
			t.Fatal("expected RootCAs to be set from the CA bundle")
		}
		if tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("must never set InsecureSkipVerify")
		}
		if tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
			t.Errorf("expected MinVersion >= TLS 1.2, got %x", tr.TLSClientConfig.MinVersion)
		}
	})

	t.Run("invalid CA is rejected", func(t *testing.T) {
		if _, err := NewDittoFSClientWithCA("https://example.svc:8080", []byte("not a pem")); err == nil {
			t.Fatal("expected an error for an invalid CA bundle")
		}
	})
}
