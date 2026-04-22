package errors

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"syscall"
	"testing"

	smithy "github.com/aws/smithy-go"

	"github.com/marmos91/dittofs/pkg/backup/destination"
)

type fakeAPIError struct {
	code string
	msg  string
}

func (e *fakeAPIError) Error() string                 { return e.msg }
func (e *fakeAPIError) ErrorCode() string             { return e.code }
func (e *fakeAPIError) ErrorMessage() string          { return e.msg }
func (e *fakeAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestClassify_NilReturnsNil(t *testing.T) {
	if got := Classify(nil); got != nil {
		t.Fatalf("Classify(nil) = %v, want nil", got)
	}
}

func TestClassify_Taxonomy(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Code
	}{
		{"invalid-access-key", &fakeAPIError{code: "InvalidAccessKeyId"}, CodeDestinationCredentialsInvalid},
		{"signature-mismatch", &fakeAPIError{code: "SignatureDoesNotMatch"}, CodeDestinationCredentialsInvalid},
		{"no-such-bucket", &fakeAPIError{code: "NoSuchBucket"}, CodeDestinationNotFound},
		{"access-denied-smithy", &fakeAPIError{code: "AccessDenied"}, CodeDestinationPermissionDenied},
		{"enospc", &fs.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}, CodeDestinationNoSpace},
		{"fs-permission", &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrPermission}, CodeDestinationPermissionDenied},
		{"fs-not-exist", &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist}, CodeDestinationNotFound},
		{"destination-permission", destination.ErrPermissionDenied, CodeDestinationPermissionDenied},
		{"destination-unavailable", destination.ErrDestinationUnavailable, CodeDestinationUnreachable},
		{"destination-throttled", destination.ErrDestinationThrottled, CodeDestinationUnreachable},
		{"destination-incompatible", destination.ErrIncompatibleConfig, CodeDestinationNotFound},
		{"dns-error", &net.DNSError{Err: "nxdomain", Name: "example.invalid"}, CodeDestinationUnreachable},
		{"opaque", errors.New("something weird"), CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err)
			if got == nil {
				t.Fatalf("Classify(%v) = nil, want %s", tc.err, tc.want)
			}
			if got.Code != tc.want {
				t.Fatalf("Classify(%v).Code = %s, want %s", tc.err, got.Code, tc.want)
			}
			// Unwrap chain preserves the original sentinel.
			if !errors.Is(got, tc.err) {
				t.Fatalf("errors.Is(classified, original) = false; chain broken")
			}
		})
	}
}

func TestClassify_Idempotent(t *testing.T) {
	be := New(CodeDestinationPathConflict, errors.New("collide"))
	wrapped := fmt.Errorf("wrapping: %w", be)
	got := Classify(wrapped)
	if got.Code != CodeDestinationPathConflict {
		t.Fatalf("Classify preserved code = %s, want %s", got.Code, CodeDestinationPathConflict)
	}
}

func TestHintFor_AllCodesCovered(t *testing.T) {
	codes := []Code{
		CodeDestinationPermissionDenied,
		CodeDestinationNotFound,
		CodeDestinationNoSpace,
		CodeDestinationUnreachable,
		CodeDestinationCredentialsInvalid,
		CodeDestinationPathConflict,
		CodeDestinationConfigInvalid,
		CodeSourceUnavailable,
		CodeBackupAlreadyRunning,
		CodeRestorePreconditionFailed,
		CodeInternal,
	}
	for _, c := range codes {
		if HintFor(c) == "" {
			t.Errorf("HintFor(%s) returned empty string", c)
		}
	}
}
