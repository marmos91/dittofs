package compression

import "errors"

var (
	// ErrUnsupportedCompressionAlgo is returned when a frame header or
	// policy specifies an algorithm byte/string this build does not
	// recognise.
	ErrUnsupportedCompressionAlgo = errors.New("compression: unsupported algorithm")

	// ErrCompressedFrameCorrupt is returned when a wire payload begins
	// with the DFCMP magic but the rest of the header (algo byte
	// uvarint orig_size) fails to parse.
	ErrCompressedFrameCorrupt = errors.New("compression: corrupt frame header")
)
