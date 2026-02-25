package types

// ============================================================================
// NSM XDR Types
// ============================================================================
//
// These types match the Open Group NSM specification.
// All structures follow the XDR encoding rules from RFC 4506.

// SMName identifies a host to monitor or query.
//
// Per Open Group specification:
//
//	struct sm_name {
//	    string mon_name<SM_MAXSTRLEN>;  // Host to monitor
//	};
//
// Used as argument to SM_STAT and SM_UNMON procedures.
type SMName struct {
	// Name is the hostname to monitor or query.
	// Max length: 1024 bytes (SM_MAXSTRLEN).
	Name string
}

// MyID contains RPC callback information for SM_NOTIFY.
//
// Per Open Group specification:
//
//	struct my_id {
//	    string my_name<SM_MAXSTRLEN>;  // Callback hostname
//	    int    my_prog;                 // RPC program number
//	    int    my_vers;                 // Program version
//	    int    my_proc;                 // Procedure number
//	};
//
// This structure tells the NSM server how to send SM_NOTIFY callbacks
// to the client when a monitored host changes state.
type MyID struct {
	// MyName is the hostname where the callback should be sent.
	// Typically the client's own hostname.
	MyName string

	// MyProg is the RPC program number for the callback.
	// Typically NLM program number (100021) for lock managers.
	MyProg uint32

	// MyVers is the program version for the callback.
	MyVers uint32

	// MyProc is the procedure number for the callback.
	// NLM uses NLM_FREE_ALL (procedure 23) to release locks.
	MyProc uint32
}

// MonID combines the monitored host and callback information.
//
// Per Open Group specification:
//
//	struct mon_id {
//	    string mon_name<SM_MAXSTRLEN>;  // Host to monitor
//	    my_id  my_id;                    // Callback info
//	};
type MonID struct {
	// MonName is the hostname to monitor.
	// Max length: 1024 bytes (SM_MAXSTRLEN).
	MonName string

	// MyID contains the callback RPC details.
	MyID MyID
}

// Mon is the argument structure for SM_MON procedure.
//
// Per Open Group specification:
//
//	struct mon {
//	    mon_id   mon_id;       // Monitor and callback info
//	    opaque   priv[16];     // Private data returned in notifications
//	};
//
// The priv field is opaque data that the server stores and returns
// unchanged in SM_NOTIFY callbacks. Clients typically store lock
// owner information or other context needed for recovery.
type Mon struct {
	// MonID combines the monitored host and callback info.
	MonID MonID

	// Priv is a 16-byte private data field.
	// Stored by the server and returned unchanged in SM_NOTIFY.
	// MUST be [16]byte (fixed array) per XDR opaque[16] encoding.
	Priv [16]byte
}

// SMStatRes is the response structure for SM_STAT and SM_MON procedures.
//
// Per Open Group specification:
//
//	struct sm_stat_res {
//	    sm_res   res_stat;     // Result code (STAT_SUCC or STAT_FAIL)
//	    int      state;        // Current NSM state counter
//	};
//
// The state field contains the current state counter of the local NSM.
// State counters are:
//   - Odd: Host is up (after restart)
//   - Even: Host went down (after crash detection)
type SMStatRes struct {
	// Result is the operation result (StatSucc or StatFail).
	Result uint32

	// State is the current NSM state counter.
	// Odd = host up, even = host down.
	State int32
}

// SMStat holds just the state number.
//
// Per Open Group specification:
//
//	struct sm_stat {
//	    int state;            // Current NSM state counter
//	};
//
// Used in some SM_STAT response variants.
type SMStat struct {
	// State is the current NSM state counter.
	State int32
}

// StatChge is the argument structure for SM_NOTIFY procedure.
//
// Per Open Group specification:
//
//	struct stat_chge {
//	    string   mon_name<SM_MAXSTRLEN>;  // Host that changed state
//	    int      state;                    // New state number
//	};
//
// Sent by NSM to notify other NSM instances that a host has restarted.
// The receiving NSM then sends callbacks to all registered monitors.
type StatChge struct {
	// MonName is the hostname that changed state.
	MonName string

	// State is the new state number after the change.
	State int32
}

// Status is the callback payload sent to registered monitors.
//
// Per Open Group specification:
//
//	struct status {
//	    string   mon_name<SM_MAXSTRLEN>;  // Host that changed state
//	    int      state;                    // New state number
//	    opaque   priv[16];                 // Client's private data
//	};
//
// This is sent via the RPC callback specified in the original SM_MON.
// The priv field contains the exact data the client provided in SM_MON,
// allowing the client to identify which locks to reclaim.
type Status struct {
	// MonName is the hostname that changed state.
	MonName string

	// State is the new state number after the change.
	State int32

	// Priv is the 16-byte private data from the original SM_MON.
	// Returned unchanged to help client identify recovery context.
	// MUST be [16]byte (fixed array) per XDR opaque[16] encoding.
	Priv [16]byte
}
