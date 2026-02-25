package types

// ============================================================================
// Reusable Test Fixtures for NFSv4.1 Types
// ============================================================================
//
// These package-level functions provide pre-built test data for use in
// per-operation test files (exchange_id_test.go, create_session_test.go, etc.).
// They are only available to test files in the same package (_test.go).

// ValidSessionId returns a non-zero session ID for testing.
func ValidSessionId() SessionId4 {
	return SessionId4{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}
}

// ValidChannelAttrs returns reasonable defaults for channel attributes.
// Values match typical Linux NFS client defaults.
func ValidChannelAttrs() ChannelAttrs {
	return ChannelAttrs{
		HeaderPadSize:         0,
		MaxRequestSize:        1049620,
		MaxResponseSize:       1049620,
		MaxResponseSizeCached: 1049620,
		MaxOperations:         16,
		MaxRequests:           64,
		RdmaIrd:               nil, // no RDMA
	}
}

// ValidClientOwner returns a test client owner with deterministic values.
func ValidClientOwner() ClientOwner4 {
	return ClientOwner4{
		Verifier: [8]byte{0xde, 0xad, 0xbe, 0xef, 0xca, 0xfe, 0xba, 0xbe},
		OwnerID:  []byte("dittofs-test-client-owner"),
	}
}

// ValidServerOwner returns a test server owner with deterministic values.
func ValidServerOwner() ServerOwner4 {
	return ServerOwner4{
		MinorID: 0,
		MajorID: []byte("dittofs-test-server"),
	}
}

// ValidNfsImplId returns a test implementation ID with "dittofs.test" domain.
func ValidNfsImplId() NfsImplId4 {
	return NfsImplId4{
		Domain: "dittofs.test",
		Name:   "DittoFS Test",
		Date:   NFS4Time{Seconds: 1700000000, Nseconds: 0},
	}
}

// ValidStateProtectNone returns SP4_NONE state protection.
func ValidStateProtectNone() StateProtect4A {
	return StateProtect4A{How: SP4_NONE}
}

// ValidStateid returns a non-zero stateid for testing (reuses existing Stateid4 type).
func ValidStateid() Stateid4 {
	return Stateid4{
		Seqid: 1,
		Other: [NFS4_OTHER_SIZE]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c},
	}
}

// ValidV41RequestContext returns a test context with valid session/slot/seq.
func ValidV41RequestContext() V41RequestContext {
	return V41RequestContext{
		SessionID:   ValidSessionId(),
		SlotID:      0,
		SequenceID:  1,
		HighestSlot: 15,
		CacheThis:   false,
	}
}
