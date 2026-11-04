package shares

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// TestXattrStreamReader_AnswersInTheIORow: a stream with content answers the
// I/O row every data operation answers, not the bare sentinel, which the
// protocol mappers would turn into a server fault.
func TestXattrStreamReader_AnswersInTheIORow(t *testing.T) {
	read := New().XattrStreamReader()
	_, err := read(context.Background(), nil, &metadata.FileAttr{Size: 4, PayloadID: "p"})
	var se *merrs.StoreError
	if !errors.As(err, &se) || se.Code != merrs.ErrIOError || !errors.Is(err, block.ErrNoContentStore) {
		t.Fatalf("stream read = %v, want a StoreError with ErrIOError wrapping block.ErrNoContentStore", err)
	}
}
