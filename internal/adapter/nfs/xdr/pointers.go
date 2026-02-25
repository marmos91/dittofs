package xdr

// ptrUint32 creates a pointer to a uint32 value.
// This is used for optional fields in authentication context.
func ptrUint32(v uint32) *uint32 {
	return &v
}
