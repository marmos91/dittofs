// Handler-level coverage for the per-share AllowMFsymlink gate and the
// MFsymlink-target path-traversal guard on CLOSE.
//
// macOS/Windows SMB clients create symlinks by writing 1067-byte MFsymlink
// (XSym) content. On CLOSE, DittoFS optionally promotes such a file to a real
// symlink for NFS interoperability. The promotion target is fully
// client-controlled, so:
//   - it is opt-in per share (AllowMFsymlink, default false), and
//   - the decoded target is rejected if it is absolute or contains `..`
//     (would escape the share root).
//
// These tests drive h.Close end-to-end against a memory metadata + block store,
// the same backends every smb-conformance profile uses.
package handlers
