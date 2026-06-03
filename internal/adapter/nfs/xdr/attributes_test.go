package xdr

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestCaptureWccAttr_NilReturnsNil(t *testing.T) {
	if got := CaptureWccAttr(nil); got != nil {
		t.Fatalf("CaptureWccAttr(nil) = %+v, want nil", got)
	}
}

func TestCaptureWccAttr_CopiesSizeAndTimes(t *testing.T) {
	mtime := time.Unix(1_700_000_000, 123).UTC()
	ctime := time.Unix(1_700_000_500, 456).UTC()

	attr := &metadata.FileAttr{
		Size:  4096,
		Mtime: mtime,
		Ctime: ctime,
	}

	got := CaptureWccAttr(attr)
	if got == nil {
		t.Fatal("CaptureWccAttr(attr) = nil, want non-nil")
	}

	if got.Size != attr.Size {
		t.Errorf("Size = %d, want %d", got.Size, attr.Size)
	}
	if got.Mtime.Seconds != uint32(mtime.Unix()) || got.Mtime.Nseconds != uint32(mtime.Nanosecond()) {
		t.Errorf("Mtime = %+v, want secs=%d nsecs=%d", got.Mtime, uint32(mtime.Unix()), uint32(mtime.Nanosecond()))
	}
	if got.Ctime.Seconds != uint32(ctime.Unix()) || got.Ctime.Nseconds != uint32(ctime.Nanosecond()) {
		t.Errorf("Ctime = %+v, want secs=%d nsecs=%d", got.Ctime, uint32(ctime.Unix()), uint32(ctime.Nanosecond()))
	}
}
