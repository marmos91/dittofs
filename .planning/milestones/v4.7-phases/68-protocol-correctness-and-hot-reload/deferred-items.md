# Deferred Items - Phase 68

## Pre-existing Issues (Out of Scope)

1. **share_hotreload_test.go:29 - Unknown field MetadataStoreName**
   - File: `pkg/controlplane/runtime/share_hotreload_test.go`
   - Issue: Uses `MetadataStoreName` but the `ShareConfig` struct field is `MetadataStore`
   - Impact: Blocks `go vet` / `golangci-lint` on the runtime package
   - Not caused by Phase 68 changes
