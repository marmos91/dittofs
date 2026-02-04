package nfs

import (
	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
)

// getNFSPort returns the NFS port from the spec or the default (2049)
func GetNFSPort(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.NFSPort != nil {
		return *dittoServer.Spec.NFSPort
	}
	return 2049
}
