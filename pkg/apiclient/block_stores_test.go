package apiclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlockStore_ListLocal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/store/block/local", r.URL.Path)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]BlockStore{
			{ID: "1", Name: "local-fs", Kind: "local", Type: "fs"},
			{ID: "2", Name: "local-mem", Kind: "local", Type: "memory"},
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	stores, err := client.ListBlockStores("local")

	require.NoError(t, err)
	assert.Len(t, stores, 2)
	assert.Equal(t, "local-fs", stores[0].Name)
	assert.Equal(t, "local", stores[0].Kind)
}

func TestBlockStore_CreateRemote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/store/block/remote", r.URL.Path)

		var req createStoreAPIRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)
		assert.Equal(t, "s3-store", req.Name)
		assert.Equal(t, "s3", req.Type)

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(BlockStore{
			ID:   "new-id",
			Name: "s3-store",
			Kind: "remote",
			Type: "s3",
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	store, err := client.CreateBlockStore("remote", &CreateStoreRequest{
		Name: "s3-store",
		Type: "s3",
	})

	require.NoError(t, err)
	assert.Equal(t, "new-id", store.ID)
	assert.Equal(t, "s3-store", store.Name)
	assert.Equal(t, "remote", store.Kind)
}

func TestBlockStore_Delete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/api/v1/store/block/local/my-store", r.URL.Path)

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	err := client.DeleteBlockStore("local", "my-store")

	require.NoError(t, err)
}

func TestBlockStore_GetRemote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/store/block/remote/s3-prod", r.URL.Path)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(BlockStore{
			ID:   "store-123",
			Name: "s3-prod",
			Kind: "remote",
			Type: "s3",
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	store, err := client.GetBlockStore("remote", "s3-prod")

	require.NoError(t, err)
	assert.Equal(t, "store-123", store.ID)
	assert.Equal(t, "s3-prod", store.Name)
}

func TestMetadataStore_ListUsesNewPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/store/metadata", r.URL.Path)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]MetadataStore{
			{ID: "1", Name: "default", Type: "memory"},
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	stores, err := client.ListMetadataStores()

	require.NoError(t, err)
	assert.Len(t, stores, 1)
	assert.Equal(t, "default", stores[0].Name)
}
