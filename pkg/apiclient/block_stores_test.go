package apiclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlockStore_List(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/store/block", r.URL.Path)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]BlockStore{
			{ID: "1", Name: "s3-prod", Type: "s3"},
			{ID: "2", Name: "mem", Type: "memory"},
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	stores, err := client.ListBlockStores()

	require.NoError(t, err)
	assert.Len(t, stores, 2)
	assert.Equal(t, "s3-prod", stores[0].Name)
	assert.Equal(t, "s3", stores[0].Type)
}

func TestBlockStore_Create(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/store/block", r.URL.Path)

		var req createStoreAPIRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)
		assert.Equal(t, "s3-store", req.Name)
		assert.Equal(t, "s3", req.Type)

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(BlockStore{
			ID:   "new-id",
			Name: "s3-store",
			Type: "s3",
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	store, err := client.CreateBlockStore(&CreateStoreRequest{
		Name: "s3-store",
		Type: "s3",
	})

	require.NoError(t, err)
	assert.Equal(t, "new-id", store.ID)
	assert.Equal(t, "s3-store", store.Name)
	assert.Equal(t, "s3", store.Type)
}

func TestBlockStore_Delete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/api/v1/store/block/my-store", r.URL.Path)

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	err := client.RemoveBlockStore("my-store")

	require.NoError(t, err)
}

func TestBlockStore_Get(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/store/block/s3-prod", r.URL.Path)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(BlockStore{
			ID:   "store-123",
			Name: "s3-prod",
			Type: "s3",
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	store, err := client.GetBlockStore("s3-prod")

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
