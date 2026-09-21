package types

// CreateContext is a single SMB2 CREATE context: a tagged blob carried on a
// CREATE request or response [MS-SMB2] 2.2.13.2 / 2.2.14.2.
//
// It lives here rather than with the CREATE handler because both the request
// decoder and the response encoder carry it, and a type shared across that
// boundary cannot sit on either side of it without pulling the other back.
type CreateContext struct {
	// Name identifies the type of create context.
	// Standard names: "MxAc", "QFid", "RqLs", etc.
	Name string

	// Data contains the context-specific data.
	Data []byte
}
