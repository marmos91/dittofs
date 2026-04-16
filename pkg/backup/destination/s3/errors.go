package s3

import (
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/marmos91/dittofs/pkg/backup/destination"
)

// isNotFound returns true when err is a 404-equivalent response:
// NoSuchKey / NoSuchBucket typed errors, a smithyhttp.ResponseError with
// 404 status, or a generic API error with one of the known codes.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nb *types.NoSuchBucket
	if errors.As(err, &nb) {
		return true
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode == http.StatusNotFound {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NoSuchBucket", "NotFound":
			return true
		}
	}
	return false
}

// classifyS3Error maps AWS SDK errors to destination package D-07 sentinels.
// Call at every SDK-call boundary. Preserves the original error via %w.
//
// Mappings (asserted by TestClassifyS3Error_MapsCodes):
//   - AccessDenied / Forbidden / InvalidAccessKeyId / SignatureDoesNotMatch → ErrPermissionDenied
//   - SlowDown / RequestLimitExceeded / ThrottlingException / HTTP 429     → ErrDestinationThrottled
//   - NoSuchBucket                                                          → ErrIncompatibleConfig
//   - HTTP 5xx / net.Error / network-class                                  → ErrDestinationUnavailable
//
// Unknown codes pass through as-is so the orchestrator can log the raw
// SDK error for diagnostics.
func classifyS3Error(err error) error {
	if err == nil {
		return nil
	}
	// Prefer the typed API-error code for first-class classification.
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "AccessDenied", "Forbidden", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return fmt.Errorf("%w: %v", destination.ErrPermissionDenied, err)
		case "SlowDown", "RequestLimitExceeded", "ThrottlingException":
			return fmt.Errorf("%w: %v", destination.ErrDestinationThrottled, err)
		case "NoSuchBucket":
			return fmt.Errorf("%w: %v", destination.ErrIncompatibleConfig, err)
		case "InternalError", "ServiceUnavailable", "RequestTimeout":
			return fmt.Errorf("%w: %v", destination.ErrDestinationUnavailable, err)
		}
	}
	// HTTP-status fallback for responses that didn't carry a typed code.
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		switch re.Response.StatusCode {
		case http.StatusTooManyRequests:
			return fmt.Errorf("%w: %v", destination.ErrDestinationThrottled, err)
		case http.StatusForbidden:
			return fmt.Errorf("%w: %v", destination.ErrPermissionDenied, err)
		}
		if re.Response.StatusCode >= 500 {
			return fmt.Errorf("%w: %v", destination.ErrDestinationUnavailable, err)
		}
	}
	// Network-class errors (DNS, connection refused, timeouts) surface as
	// transient — orchestrator may retry.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return fmt.Errorf("%w: %v", destination.ErrDestinationUnavailable, err)
	}
	return err
}
