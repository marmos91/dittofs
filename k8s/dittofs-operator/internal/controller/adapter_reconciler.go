/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const defaultPollingInterval = 30 * time.Second

// getPollingInterval reads the adapter polling interval from the CRD spec.
// Falls back to defaultPollingInterval (30s) if nil, empty, invalid, or non-positive.
func getPollingInterval(ds *dittoiov1alpha1.DittoServer) time.Duration {
	if ds.Spec.AdapterDiscovery == nil || ds.Spec.AdapterDiscovery.PollingInterval == "" {
		return defaultPollingInterval
	}

	d, err := time.ParseDuration(ds.Spec.AdapterDiscovery.PollingInterval)
	if err != nil || d <= 0 {
		return defaultPollingInterval
	}

	return d
}

// getAuthenticatedClient reads the operator credentials Secret and returns an authenticated DittoFSClient.
func (r *DittoServerReconciler) getAuthenticatedClient(ctx context.Context, ds *dittoiov1alpha1.DittoServer) (*DittoFSClient, error) {
	credSecret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: ds.Namespace,
		Name:      ds.GetOperatorCredentialsSecretName(),
	}, credSecret); err != nil {
		return nil, fmt.Errorf("failed to get operator credentials secret: %w", err)
	}

	serverURL := string(credSecret.Data["server-url"])
	accessToken := string(credSecret.Data["access-token"])

	if serverURL == "" || accessToken == "" {
		return nil, fmt.Errorf("operator credentials secret is missing server-url or access-token")
	}

	apiClient := NewDittoFSClient(serverURL)
	apiClient.SetToken(accessToken)

	return apiClient, nil
}

// reconcileAdapters polls the DittoFS API for adapter state and stores results.
// On any error, it preserves existing adapter state (DISC-03 safety guard).
func (r *DittoServerReconciler) reconcileAdapters(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	pollingInterval := getPollingInterval(dittoServer)

	// Get authenticated client
	apiClient, err := r.getAuthenticatedClient(ctx, dittoServer)
	if err != nil {
		logger.Info("Failed to get authenticated client for adapter polling, preserving existing state", "error", err.Error())
		return ctrl.Result{RequeueAfter: pollingInterval}, nil
	}

	// Poll adapter list
	adapters, err := apiClient.ListAdapters()
	if err != nil {
		logger.Info("Adapter polling failed, preserving existing state", "error", err.Error())
		return ctrl.Result{RequeueAfter: pollingInterval}, nil
	}

	// Store successful result
	r.setLastKnownAdapters(dittoServer, adapters)

	logger.V(1).Info("Adapter polling succeeded", "adapterCount", len(adapters), "nextPoll", pollingInterval)

	return ctrl.Result{RequeueAfter: pollingInterval}, nil
}

// setLastKnownAdapters stores the last successful adapter poll result.
func (r *DittoServerReconciler) setLastKnownAdapters(ds *dittoiov1alpha1.DittoServer, adapters []AdapterInfo) {
	r.adaptersMu.Lock()
	defer r.adaptersMu.Unlock()

	if r.lastKnownAdapters == nil {
		r.lastKnownAdapters = make(map[string][]AdapterInfo)
	}

	key := ds.Namespace + "/" + ds.Name
	r.lastKnownAdapters[key] = adapters
}

// activeAdaptersByType returns a map of enabled+running adapters keyed by type.
func activeAdaptersByType(adapters []AdapterInfo) map[string]AdapterInfo {
	result := make(map[string]AdapterInfo)
	for _, a := range adapters {
		if a.Enabled && a.Running {
			result[a.Type] = a
		}
	}
	return result
}

// getLastKnownAdapters returns the last known adapter state for a DittoServer.
// Returns nil if no successful poll has occurred yet.
func (r *DittoServerReconciler) getLastKnownAdapters(ds *dittoiov1alpha1.DittoServer) []AdapterInfo {
	r.adaptersMu.RLock()
	defer r.adaptersMu.RUnlock()

	if r.lastKnownAdapters == nil {
		return nil
	}

	key := ds.Namespace + "/" + ds.Name
	return r.lastKnownAdapters[key]
}
