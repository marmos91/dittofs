package apiclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Refs #514 — apiclient wire-format guards for AclFlagInheritedCanonicalization.

// TestShare_JSON_IncludesAclFlagInheritedCanonicalization mirrors the
// `enabled` no-omitempty regression test: false must be emitted explicitly
// so consumers can render the disabled state.
func TestShare_JSON_IncludesAclFlagInheritedCanonicalization(t *testing.T) {
	b, err := json.Marshal(Share{AclFlagInheritedCanonicalization: true})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"acl_flag_inherited_canonicalization":true`,
		"Share JSON must emit acl_flag_inherited_canonicalization=true")

	b2, err := json.Marshal(Share{AclFlagInheritedCanonicalization: false})
	require.NoError(t, err)
	assert.Contains(t, string(b2), `"acl_flag_inherited_canonicalization":false`,
		"Share JSON must emit acl_flag_inherited_canonicalization=false (no omitempty)")

	var s Share
	require.NoError(t, json.Unmarshal([]byte(`{"acl_flag_inherited_canonicalization":false}`), &s))
	assert.False(t, s.AclFlagInheritedCanonicalization,
		"unmarshal of explicit false must keep AclFlagInheritedCanonicalization=false")
}

// TestCreateShareRequest_JSON_AclFlagOmitsWhenNil confirms the pointer
// semantics: nil → field absent (server uses default true); non-nil →
// emitted with the chosen value.
func TestCreateShareRequest_JSON_AclFlagOmitsWhenNil(t *testing.T) {
	b, err := json.Marshal(CreateShareRequest{Name: "/x"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "acl_flag_inherited_canonicalization",
		"omitempty must suppress the field when callers leave it unset")

	falseV := false
	b2, err := json.Marshal(CreateShareRequest{
		Name:                             "/x",
		AclFlagInheritedCanonicalization: &falseV,
	})
	require.NoError(t, err)
	assert.Contains(t, string(b2), `"acl_flag_inherited_canonicalization":false`,
		"explicit false must serialize on CreateShareRequest")
}

// TestUpdateShareRequest_JSON_AclFlagOmitsWhenNil mirrors the Create guard
// on the Update DTO.
func TestUpdateShareRequest_JSON_AclFlagOmitsWhenNil(t *testing.T) {
	b, err := json.Marshal(UpdateShareRequest{})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "acl_flag_inherited_canonicalization",
		"omitempty must suppress the field when callers leave it unset")

	falseV := false
	b2, err := json.Marshal(UpdateShareRequest{
		AclFlagInheritedCanonicalization: &falseV,
	})
	require.NoError(t, err)
	assert.Contains(t, string(b2), `"acl_flag_inherited_canonicalization":false`,
		"explicit false must serialize on UpdateShareRequest")
}

// TestCreateShare_SendsAclFlagInheritedCanonicalizationFalse verifies the
// apiclient's CreateShare PUTs the explicit-false flag on the wire — this
// is the dfsctl → REST contract Test for Task T2.
func TestCreateShare_SendsAclFlagInheritedCanonicalizationFalse(t *testing.T) {
	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/shares", r.URL.Path)
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		receivedBody = b

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Share{
			ID:                               "new-id",
			Name:                             "/x",
			AclFlagInheritedCanonicalization: false,
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")

	falseV := false
	share, err := client.CreateShare(&CreateShareRequest{
		Name:                             "/x",
		MetadataStoreID:                  "meta",
		LocalBlockStore:                  "bs",
		AclFlagInheritedCanonicalization: &falseV,
	})
	require.NoError(t, err)
	assert.False(t, share.AclFlagInheritedCanonicalization)
	assert.True(t, bytes.Contains(receivedBody, []byte(`"acl_flag_inherited_canonicalization":false`)),
		"wire payload must contain acl_flag_inherited_canonicalization:false; got %s", receivedBody)
}
