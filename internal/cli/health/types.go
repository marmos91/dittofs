// Package health provides shared types for health check responses.
package health

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Response represents the simplified /health liveness response.
type Response struct {
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	Data      struct {
		Service        string `json:"service"`
		StartedAt      string `json:"started_at"`
		Uptime         string `json:"uptime"`
		UptimeSec      int64  `json:"uptime_sec"`
		ControlPlaneDB string `json:"control_plane_db"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// StatusReport mirrors pkg/health.Report for CLI deserialization.
type StatusReport struct {
	Status    string    `json:"status"`
	Message   string    `json:"message,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	LatencyMs int64     `json:"latency_ms,omitempty"`
}

// ShareListItem is a minimal struct to deserialize share list entries with status.
type ShareListItem struct {
	Name   string       `json:"name"`
	Status StatusReport `json:"status"`
}

// BlockStoreListItem deserializes block store list entries with status.
type BlockStoreListItem struct {
	Name   string       `json:"name"`
	Kind   string       `json:"kind"`
	Type   string       `json:"type"`
	Status StatusReport `json:"status"`
}

// MetadataStoreListItem deserializes metadata store list entries with status.
type MetadataStoreListItem struct {
	Name   string       `json:"name"`
	Type   string       `json:"type"`
	Status StatusReport `json:"status"`
}

// AdapterListItem deserializes adapter list entries with status.
type AdapterListItem struct {
	Type    string       `json:"type"`
	Running bool         `json:"running"`
	Status  StatusReport `json:"status"`
}

// Entities holds per-entity status lists fetched from the API.
type Entities struct {
	Shares      []ShareListItem         `json:"shares,omitempty" yaml:"shares,omitempty"`
	BlockStores []BlockStoreListItem    `json:"block_stores,omitempty" yaml:"block_stores,omitempty"`
	MetaStores  []MetadataStoreListItem `json:"metadata_stores,omitempty" yaml:"metadata_stores,omitempty"`
	Adapters    []AdapterListItem       `json:"adapters,omitempty" yaml:"adapters,omitempty"`
	Errors      []string                `json:"errors,omitempty" yaml:"errors,omitempty"`
}

// FetchEntities fetches per-entity status from the list endpoints in parallel.
// The baseURL should be the API v1 base (e.g. "http://localhost:8080/api/v1").
func FetchEntities(client *http.Client, baseURL, token string) Entities {
	baseURL = strings.TrimRight(baseURL, "/")
	var ent Entities
	var wg sync.WaitGroup
	var mu sync.Mutex

	doGet := func(url string, target any) error {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			return fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(snippet)))
		}
		return json.NewDecoder(resp.Body).Decode(target)
	}

	wg.Add(4)

	go func() {
		defer wg.Done()
		var shares []ShareListItem
		if err := doGet(baseURL+"/shares", &shares); err == nil {
			mu.Lock()
			ent.Shares = shares
			mu.Unlock()
		} else {
			mu.Lock()
			ent.Errors = append(ent.Errors, fmt.Sprintf("shares: %v", err))
			mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		var allStores []BlockStoreListItem

		var local []BlockStoreListItem
		if err := doGet(baseURL+"/store/block/local", &local); err == nil {
			allStores = append(allStores, local...)
		} else {
			mu.Lock()
			ent.Errors = append(ent.Errors, fmt.Sprintf("block stores (local): %v", err))
			mu.Unlock()
		}

		var remote []BlockStoreListItem
		if err := doGet(baseURL+"/store/block/remote", &remote); err == nil {
			allStores = append(allStores, remote...)
		} else {
			mu.Lock()
			ent.Errors = append(ent.Errors, fmt.Sprintf("block stores (remote): %v", err))
			mu.Unlock()
		}

		if len(allStores) > 0 {
			mu.Lock()
			ent.BlockStores = allStores
			mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		var stores []MetadataStoreListItem
		if err := doGet(baseURL+"/store/metadata", &stores); err == nil {
			mu.Lock()
			ent.MetaStores = stores
			mu.Unlock()
		} else {
			mu.Lock()
			ent.Errors = append(ent.Errors, fmt.Sprintf("metadata stores: %v", err))
			mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		var adapters []AdapterListItem
		if err := doGet(baseURL+"/adapters", &adapters); err == nil {
			mu.Lock()
			ent.Adapters = adapters
			mu.Unlock()
		} else {
			mu.Lock()
			ent.Errors = append(ent.Errors, fmt.Sprintf("adapters: %v", err))
			mu.Unlock()
		}
	}()

	wg.Wait()
	return ent
}
