package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// detachFromRequest builds a context for a long-running handler that must
// outlive the control plane's short global request deadline (chi
// middleware.Timeout / write_timeout, ~30s) while still stopping if the client
// genuinely disconnects.
//
// The returned context is detached from the request deadline via
// context.WithoutCancel, so middleware.Timeout cannot abort the work. When
// total > 0 it carries that wall-clock budget; when total <= 0 it has no
// deadline of its own and the caller is expected to supply its own bound (e.g.
// an inactivity watchdog). Either way a real client disconnect — the request
// context reporting Canceled, as opposed to the DeadlineExceeded fired by
// middleware.Timeout — cancels it.
//
// The returned CancelFunc both unregisters the disconnect watcher and cancels
// the context; it is idempotent, must be deferred, and may also be called early
// (e.g. by a watchdog) to abort the work.
func detachFromRequest(r *http.Request, total time.Duration) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(r.Context())

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if total > 0 {
		ctx, cancel = context.WithTimeout(base, total)
	} else {
		ctx, cancel = context.WithCancel(base)
	}

	stop := context.AfterFunc(r.Context(), func() {
		if r.Context().Err() == context.Canceled {
			cancel()
		}
	})

	return ctx, func() {
		stop()
		cancel()
	}
}

// redactedSecret is the sentinel substituted for secret values in store
// config blobs returned on read paths.
const redactedSecret = "********"

// redactSecretJSON parses a stored store-config JSON object and replaces the
// values of secret-bearing keys with a fixed sentinel, returning the
// re-serialized blob. It is applied on READ paths only (GET/List/Create/Update
// responses) so the S3 secret_access_key and postgres password never leave the
// process; Create/Update still accept plaintext input.
//
// A key is considered secret if it is exactly "secret_access_key"/"password"
// or, by convention, contains "secret"/"password" or ends in "_key" (e.g.
// "access_key", "api_key"). Matching is case-insensitive. Redaction recurses
// into nested objects and arrays.
//
// If the blob is empty or not valid JSON, it is returned unchanged — the goal
// is to never WIDEN exposure, and an unparseable blob carries no addressable
// secret key for us to mask. The caller stores well-formed JSON, so this is a
// defensive no-op in practice.
func redactSecretJSON(blob string) string {
	if strings.TrimSpace(blob) == "" {
		return blob
	}

	var decoded any
	if err := json.Unmarshal([]byte(blob), &decoded); err != nil {
		return blob
	}

	redacted := redactSecretValue(decoded)

	out, err := json.Marshal(redacted)
	if err != nil {
		return blob
	}
	return string(out)
}

// redactedConfigRaw returns a store-config blob, secrets redacted, as a
// json.RawMessage for embedding directly in a response body. Emitting the
// config as a JSON object (rather than a JSON-encoded string) is the contract
// apiclient expects: its BlockStore.Config is a json.RawMessage, so a
// stringified blob decodes to a quoted literal that silently fails to unmarshal
// into a config map — which dropped every field on a partial `store block
// remote edit` and forced re-passing the whole S3 config. A blank or
// whitespace-only blob yields a nil RawMessage so the `omitempty` field is
// omitted instead of serializing as invalid JSON. A non-empty blob that is not
// valid JSON is also dropped rather than embedded: a json.RawMessage of invalid
// bytes makes the streaming encoder fail and silently truncate the whole
// response, so omitting the field is the safer degradation.
func redactedConfigRaw(blob string) json.RawMessage {
	redacted := redactSecretJSON(blob)
	if strings.TrimSpace(redacted) == "" || !json.Valid([]byte(redacted)) {
		return nil
	}
	return json.RawMessage(redacted)
}

// redactSecretValue walks an arbitrary JSON value, redacting secret-bearing
// keys in any object it encounters.
func redactSecretValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			if isSecretKey(k) {
				val[k] = redactedSecret
				continue
			}
			val[k] = redactSecretValue(child)
		}
		return val
	case []any:
		for i, child := range val {
			val[i] = redactSecretValue(child)
		}
		return val
	default:
		return v
	}
}

// isSecretKey reports whether a config key names a secret value.
func isSecretKey(key string) bool {
	k := strings.ToLower(key)
	return strings.Contains(k, "secret") ||
		strings.Contains(k, "password") ||
		strings.HasSuffix(k, "_key")
}

// mergeRedactedSecrets reconciles an incoming store-config blob (newBlob)
// against the currently stored one (oldBlob): any value in newBlob equal to
// the redaction sentinel is replaced with the corresponding value from
// oldBlob. This makes the read-then-write pattern safe — because read paths
// emit "********" for secrets, a client (CLI, operator UI) that fetches a
// config, tweaks a non-secret field, and PUTs it back would otherwise persist
// the sentinel and destroy the real credential. Reconciling here, at the
// Update handler, fixes the whole class regardless of which client resends a
// redacted blob.
//
// If either blob is empty or not a JSON object, newBlob is returned unchanged
// (Create-style full replacement still works, and a non-object blob carries no
// addressable key to reconcile).
func mergeRedactedSecrets(oldBlob, newBlob string) string {
	if strings.TrimSpace(newBlob) == "" || strings.TrimSpace(oldBlob) == "" {
		return newBlob
	}

	var oldObj, newObj map[string]any
	if err := json.Unmarshal([]byte(oldBlob), &oldObj); err != nil {
		return newBlob
	}
	if err := json.Unmarshal([]byte(newBlob), &newObj); err != nil {
		return newBlob
	}

	if !restoreRedactedValues(oldObj, newObj) {
		// No sentinel present: nothing to reconcile, avoid re-serializing.
		return newBlob
	}

	out, err := json.Marshal(newObj)
	if err != nil {
		return newBlob
	}
	return string(out)
}

// restoreRedactedValues walks newObj, replacing any sentinel value with the
// value at the same key in oldObj. It descends into both nested objects
// (map[string]any) and arrays ([]any), so a redacted secret nested inside an
// array is restored rather than clobbering a real secret. Returns true if any
// substitution was made.
func restoreRedactedValues(oldObj, newObj map[string]any) bool {
	changed := false
	for k, nv := range newObj {
		ov, ok := oldObj[k]
		switch nvt := nv.(type) {
		case string:
			if nvt == redactedSecret && ok {
				newObj[k] = ov
				changed = true
			}
		case map[string]any:
			if ovm, isMap := ov.(map[string]any); isMap {
				if restoreRedactedValues(ovm, nvt) {
					changed = true
				}
			}
		case []any:
			if ova, isArr := ov.([]any); isArr {
				if restoreRedactedArray(ova, nvt) {
					changed = true
				}
			}
		}
	}
	return changed
}

// restoreRedactedArray walks newArr positionally against oldArr, restoring
// sentinel values nested inside array elements. Returns true if any
// substitution was made.
func restoreRedactedArray(oldArr, newArr []any) bool {
	changed := false
	for i, nv := range newArr {
		if i >= len(oldArr) {
			break
		}
		ov := oldArr[i]
		switch nvt := nv.(type) {
		case string:
			if nvt == redactedSecret {
				newArr[i] = ov
				changed = true
			}
		case map[string]any:
			if ovm, isMap := ov.(map[string]any); isMap {
				if restoreRedactedValues(ovm, nvt) {
					changed = true
				}
			}
		case []any:
			if ova, isArr := ov.([]any); isArr {
				if restoreRedactedArray(ova, nvt) {
					changed = true
				}
			}
		}
	}
	return changed
}

// maxRequestBodyBytes is the upper bound on any JSON request body accepted
// by the control-plane API. 1 MiB is well above the largest legitimate
// payload (share config + ACL) and prevents OOM / DoS via unbounded reads.
const maxRequestBodyBytes = 1 << 20 // 1 MiB

// decodeJSONBody decodes a JSON request body into the provided pointer.
// The body is capped at maxRequestBodyBytes; exceeding the limit yields a
// 413 response. Any other decode failure yields a 400 response.
// Returns true if successful; error response is written automatically.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			WriteProblem(w, http.StatusRequestEntityTooLarge,
				"Request Entity Too Large",
				"request body exceeds the 1 MiB limit")
			return false
		}
		BadRequest(w, "Invalid request body")
		return false
	}
	return true
}

// MapStoreError maps a control plane store error to an HTTP status code and message.
func MapStoreError(err error) (int, string) {
	// Not found errors -> 404
	switch {
	case errors.Is(err, models.ErrUserNotFound):
		return http.StatusNotFound, "User not found"
	case errors.Is(err, models.ErrGroupNotFound):
		return http.StatusNotFound, "Group not found"
	case errors.Is(err, models.ErrShareNotFound):
		return http.StatusNotFound, "Share not found"
	case errors.Is(err, models.ErrStoreNotFound):
		return http.StatusNotFound, "Store not found"
	case errors.Is(err, models.ErrAdapterNotFound):
		return http.StatusNotFound, "Adapter not found"
	case errors.Is(err, models.ErrSettingNotFound):
		return http.StatusNotFound, "Setting not found"
	case errors.Is(err, models.ErrNetgroupNotFound):
		return http.StatusNotFound, "Netgroup not found"

	// Duplicate/conflict errors -> 409
	case errors.Is(err, models.ErrDuplicateUser):
		return http.StatusConflict, "User already exists"
	case errors.Is(err, models.ErrDuplicateGroup):
		return http.StatusConflict, "Group already exists"
	case errors.Is(err, models.ErrDuplicateShare):
		return http.StatusConflict, "Share already exists"
	case errors.Is(err, models.ErrDuplicateStore):
		return http.StatusConflict, "Store already exists"
	case errors.Is(err, models.ErrDuplicateAdapter):
		return http.StatusConflict, "Adapter already exists"
	case errors.Is(err, models.ErrDuplicateNetgroup):
		return http.StatusConflict, "Netgroup already exists"
	case errors.Is(err, models.ErrStoreInUse):
		return http.StatusConflict, "Store is referenced by shares"
	case errors.Is(err, models.ErrNetgroupInUse):
		return http.StatusConflict, "Netgroup is referenced by shares"

	// Forbidden errors -> 403
	case errors.Is(err, models.ErrUserDisabled):
		return http.StatusForbidden, "User account is disabled"
	case errors.Is(err, models.ErrGuestDisabled):
		return http.StatusForbidden, "Guest access is disabled"

	default:
		return http.StatusInternalServerError, "Internal server error"
	}
}

// HandleStoreError maps a store error to an HTTP response and writes it.
func HandleStoreError(w http.ResponseWriter, err error) {
	status, msg := MapStoreError(err)
	WriteProblem(w, status, http.StatusText(status), msg)
}
