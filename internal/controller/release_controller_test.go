/*
Copyright 2026.

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/entr0pian/release-operator/api/v1alpha1"
)

var _ = Describe("Release Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		release := &platformv1alpha1.Release{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Release")
			err := k8sClient.Get(ctx, typeNamespacedName, release)
			if err != nil && errors.IsNotFound(err) {
				resource := &platformv1alpha1.Release{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: platformv1alpha1.ReleaseSpec{
						ComponentRef: platformv1alpha1.ComponentReference{Name: "does-not-exist"},
						Environment:  "dev",
						Version:      "1.0.0",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &platformv1alpha1.Release{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Release")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should requeue rather than error when the referenced Component doesn't exist", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ReleaseReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &platformv1alpha1.Release{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			cond := findReadyCondition(updated)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal("ComponentNotFound"))
		})
	})

	Context("resolveComponent", func() {
		const releaseNS = "dev"

		ctx := context.Background()
		reconciler := &ReleaseReconciler{}

		BeforeEach(func() {
			reconciler.Client = k8sClient
			reconciler.Scheme = k8sClient.Scheme()

			for _, ns := range []string{releaseNS, componentNamespace} {
				err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
				if err != nil && !errors.IsAlreadyExists(err) {
					Expect(err).NotTo(HaveOccurred())
				}
			}
		})

		AfterEach(func() {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(componentGVK)
			Expect(k8sClient.List(ctx, list, client.InNamespace(componentNamespace))).To(Succeed())
			for i := range list.Items {
				Expect(k8sClient.Delete(ctx, &list.Items[i])).To(Succeed())
			}
		})

		newRelease := func(name string) *platformv1alpha1.Release {
			return &platformv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: releaseNS},
				Spec: platformv1alpha1.ReleaseSpec{
					ComponentRef: platformv1alpha1.ComponentReference{Name: "checkout"},
					Environment:  "dev",
					Version:      "1.0.0",
				},
			}
		}

		It("is not ready, with no error, when no Component exists in componentNamespace", func() {
			release := newRelease("missing-component")
			_, ready, err := reconciler.resolveComponent(ctx, release)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready).To(BeFalse())
		})

		It("resolves a Component from componentNamespace even though the Release lives in a different namespace", func() {
			component := &unstructured.Unstructured{}
			component.SetGroupVersionKind(componentGVK)
			component.SetName("checkout")
			component.SetNamespace(componentNamespace)
			Expect(unstructured.SetNestedMap(component.Object, map[string]any{
				"owner":      "checkout-team",
				"repository": map[string]any{},
			}, "spec")).To(Succeed())
			Expect(k8sClient.Create(ctx, component)).To(Succeed())

			Expect(unstructured.SetNestedMap(component.Object, map[string]any{
				"url":   "https://github.com/entr0pian/checkout",
				"ready": true,
			}, "status", "repository")).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, component)).To(Succeed())

			release := newRelease("ready-component")
			repoURL, ready, err := reconciler.resolveComponent(ctx, release)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready).To(BeTrue())
			Expect(repoURL).To(Equal("https://github.com/entr0pian/checkout.git"))
		})
	})

	Context("resolveDatabaseBinding", func() {
		const ns = "default"

		ctx := context.Background()
		reconciler := &ReleaseReconciler{}

		BeforeEach(func() {
			reconciler.Client = k8sClient
			reconciler.Scheme = k8sClient.Scheme()
		})

		newRelease := func(name string, db *platformv1alpha1.DatabaseBinding) *platformv1alpha1.Release {
			return &platformv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: platformv1alpha1.ReleaseSpec{
					ComponentRef: platformv1alpha1.ComponentReference{Name: "checkout"},
					Environment:  "dev",
					Version:      "1.0.0",
					Bindings: platformv1alpha1.ReleaseBindings{
						Database: db,
					},
				},
			}
		}

		createDatabase := func(name string) *unstructured.Unstructured {
			database := &unstructured.Unstructured{}
			database.SetGroupVersionKind(databaseGVK)
			database.SetName(name)
			database.SetNamespace(ns)
			Expect(unstructured.SetNestedMap(database.Object, map[string]any{
				"componentRef": map[string]any{"name": "checkout"},
				"dbName":       "checkout",
			}, "spec")).To(Succeed())
			Expect(k8sClient.Create(ctx, database)).To(Succeed())
			return database
		}

		AfterEach(func() {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(databaseGVK)
			Expect(k8sClient.List(ctx, list, client.InNamespace(ns))).To(Succeed())
			for i := range list.Items {
				Expect(k8sClient.Delete(ctx, &list.Items[i])).To(Succeed())
			}
		})

		It("returns ready with no secret when the binding isn't declared", func() {
			release := newRelease("no-binding", nil)
			secretName, ready, err := reconciler.resolveDatabaseBinding(ctx, release)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready).To(BeTrue())
			Expect(secretName).To(BeEmpty())
		})

		It("returns ready with no secret when the binding is disabled", func() {
			release := newRelease("disabled-binding", &platformv1alpha1.DatabaseBinding{Enabled: false})
			secretName, ready, err := reconciler.resolveDatabaseBinding(ctx, release)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready).To(BeTrue())
			Expect(secretName).To(BeEmpty())
		})

		It("is not ready, with no error, when enabled but ref is empty", func() {
			release := newRelease("empty-ref", &platformv1alpha1.DatabaseBinding{Enabled: true})
			_, ready, err := reconciler.resolveDatabaseBinding(ctx, release)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready).To(BeFalse())
		})

		It("is not ready, with no error, when the referenced Database doesn't exist", func() {
			release := newRelease("missing-db", &platformv1alpha1.DatabaseBinding{Enabled: true, Ref: "checkout-db"})
			_, ready, err := reconciler.resolveDatabaseBinding(ctx, release)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready).To(BeFalse())
		})

		It("is not ready, with no error, when the Database hasn't published a connectionSecretRef yet", func() {
			createDatabase("checkout-db")
			release := newRelease("pending-db", &platformv1alpha1.DatabaseBinding{Enabled: true, Ref: "checkout-db"})
			_, ready, err := reconciler.resolveDatabaseBinding(ctx, release)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready).To(BeFalse())
		})

		It("resolves the actual Secret name from status.connectionSecretRef, not a naming convention", func() {
			database := createDatabase("checkout-db")
			Expect(unstructured.SetNestedMap(database.Object, map[string]any{
				"name":      "checkout-db-connection",
				"namespace": ns,
			}, "status", "connectionSecretRef")).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, database)).To(Succeed())

			release := newRelease("ready-db", &platformv1alpha1.DatabaseBinding{Enabled: true, Ref: "checkout-db"})
			secretName, ready, err := reconciler.resolveDatabaseBinding(ctx, release)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready).To(BeTrue())
			Expect(secretName).To(Equal("checkout-db-connection"))
		})

		It("errors when connectionSecretRef.namespace doesn't match the Release's namespace", func() {
			database := createDatabase("checkout-db")
			Expect(unstructured.SetNestedMap(database.Object, map[string]any{
				"name":      "checkout-db-connection",
				"namespace": "some-other-namespace",
			}, "status", "connectionSecretRef")).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, database)).To(Succeed())

			release := newRelease("mismatched-namespace-db", &platformv1alpha1.DatabaseBinding{Enabled: true, Ref: "checkout-db"})
			_, _, err := reconciler.resolveDatabaseBinding(ctx, release)
			Expect(err).To(HaveOccurred())
		})
	})
})

func findReadyCondition(release *platformv1alpha1.Release) *metav1.Condition {
	for i := range release.Status.Conditions {
		if release.Status.Conditions[i].Type == readyConditionType {
			return &release.Status.Conditions[i]
		}
	}
	return nil
}
