package nfs

import (
	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
)

// DefaultNFSPort is the default NFS port for DittoFS (non-privileged)
const DefaultNFSPort = 12049

// GetNFSPort returns the NFS port from the spec or the default.
func GetNFSPort(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.NFSPort != nil {
		return *dittoServer.Spec.NFSPort
	}
	return DefaultNFSPort
}
