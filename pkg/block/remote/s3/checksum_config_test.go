package s3

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// TestNewFromConfig_DisablesSDKChecksums pins #1466: block content is
// BLAKE3-verified end-to-end, so the S3 client must run with the aws-sdk-go-v2
// flexible-checksum layer set to WhenRequired. Left at the SDK default
// (WhenSupported) every PutObject is wrapped in an aws-chunked streaming-trailer
// encoding with a full CRC32 pass over each ~16 MiB block — redundant CPU + wire
// framing, and an interop friction point with non-AWS S3 endpoints. This guards
// against a future SDK default change or an accidental removal of the option.
func TestNewFromConfig_DisablesSDKChecksums(t *testing.T) {
	store, err := NewFromConfig(context.Background(), Config{
		Bucket:    "b",
		Region:    "us-east-1",
		Endpoint:  "http://127.0.0.1:1", // never dialed — client construction is lazy
		AccessKey: "ak",
		SecretKey: "sk",
	})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	defer func() { _ = store.Close() }()

	opts := store.client.Options()
	if opts.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
		t.Errorf("RequestChecksumCalculation = %v, want WhenRequired (else PUTs use aws-chunked + CRC32)", opts.RequestChecksumCalculation)
	}
	if opts.ResponseChecksumValidation != aws.ResponseChecksumValidationWhenRequired {
		t.Errorf("ResponseChecksumValidation = %v, want WhenRequired", opts.ResponseChecksumValidation)
	}
}
