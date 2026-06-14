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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
)

// These tests exercise the deletion/finalizer path against envtest, which has
// real resourceVersion (optimistic-concurrency) semantics. They guard against
// the regression where the Phase=Deleting status write bumps the shared RV and
// the subsequent finalizer-removal Update 409-conflicts forever.
var _ = Describe("DittoServer deletion", func() {
	const namespace = "default"

	newReconciler := func() *DittoServerReconciler {
		return &DittoServerReconciler{
			Client:   k8sClient,
			Scheme:   scheme.Scheme,
			Recorder: record.NewFakeRecorder(100),
		}
	}

	newDittoServer := func(name string) *dittoiov1alpha1.DittoServer {
		return &dittoiov1alpha1.DittoServer{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: dittoiov1alpha1.DittoServerSpec{
				Storage: dittoiov1alpha1.StorageSpec{
					MetadataSize: "1Gi",
				},
			},
		}
	}

	It("removes the finalizer and deletes the object after a Phase=Deleting status write", func() {
		ds := newDittoServer("delete-finalizer-test")
		Expect(k8sClient.Create(ctx, ds)).To(Succeed())

		key := client.ObjectKeyFromObject(ds)

		// Add the finalizer the way the controller does.
		Expect(k8sClient.Get(ctx, key, ds)).To(Succeed())
		controllerutil.AddFinalizer(ds, finalizerName)
		Expect(k8sClient.Update(ctx, ds)).To(Succeed())

		// Delete the object: with the finalizer present it lingers in
		// Terminating until the finalizer is removed.
		Expect(k8sClient.Delete(ctx, ds)).To(Succeed())

		r := newReconciler()
		req := ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: namespace,
			Name:      ds.Name,
		}}

		// First reconcile: writes Phase=Deleting (bumping RV) AND must still
		// succeed at removing the finalizer on a freshly-fetched object.
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())

		// The object must be fully gone (finalizer removed -> GC completes).
		Eventually(func() bool {
			err := k8sClient.Get(ctx, key, &dittoiov1alpha1.DittoServer{})
			return apierrors.IsNotFound(err)
		}, "10s", "200ms").Should(BeTrue())
	})

	It("force-removes the finalizer without conflict on the cleanup-timeout path", func() {
		ds := newDittoServer("delete-force-test")
		Expect(k8sClient.Create(ctx, ds)).To(Succeed())

		key := client.ObjectKeyFromObject(ds)
		Expect(k8sClient.Get(ctx, key, ds)).To(Succeed())
		controllerutil.AddFinalizer(ds, finalizerName)
		Expect(k8sClient.Update(ctx, ds)).To(Succeed())
		Expect(k8sClient.Delete(ctx, ds)).To(Succeed())

		// Re-fetch so DeletionTimestamp is populated, then back-date it past the
		// cleanup timeout to drive the force-removal branch.
		Expect(k8sClient.Get(ctx, key, ds)).To(Succeed())
		backdated := metav1.NewTime(metav1.Now().Add(-2 * cleanupTimeout))
		ds.DeletionTimestamp = &backdated

		r := newReconciler()
		requeue, err := r.handleDeletion(ctx, ds)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeFalse())

		Eventually(func() bool {
			err := k8sClient.Get(ctx, key, &dittoiov1alpha1.DittoServer{})
			return apierrors.IsNotFound(err)
		}, "10s", "200ms").Should(BeTrue())
	})

	It("is idempotent when the finalizer is already absent", func() {
		// removeFinalizer on a non-existent object must not error.
		r := newReconciler()
		ds := newDittoServer("delete-absent-test")
		Expect(r.removeFinalizer(ctx, ds)).To(Succeed())
	})
})
