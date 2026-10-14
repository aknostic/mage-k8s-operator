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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// ensureDatabase drives the three discriminated-union modes of
// spec.database (inline / ref / external). Each branch has a different
// readiness path and a different credential source. Envtest is the
// cheapest place to exercise all three without spinning up a real
// MariaDB; the fake apiserver lets us inject MariaDB status conditions
// and Secret payloads that would otherwise be produced by the upstream
// operator or a human admin.
var _ = Describe("ensureDatabase", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	// Fresh namespace per test keeps resources from leaking between
	// cases and makes failure diagnosis easier. We rely on GenerateName
	// because envtest has no namespace controller — Delete just marks
	// namespaces Terminating forever, so name reuse collides. The
	// suite teardown stops the apiserver altogether; per-test cleanup
	// would only add noise without freeing anything.
	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensuredb-"},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		reconciler = &MageStoreReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Capabilities: fullCapabilities(),
		}
		store = &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: namespace},
			Spec: magev1alpha1.MageStoreSpec{
				Image: magev1alpha1.MageStoreImage{
					Repository: "registry.example.com/test/magento",
					Tag:        "2.4.8",
				},
			},
		}
	})

	Context("external mode", func() {
		// External mode is the only branch where credential plumbing
		// lives entirely in ensureDatabase — no MariaDB CR to chase.
		// The three sub-cases (Secret missing / keys missing / keys
		// present) exercise readDatabaseSecretKey's error-condition
		// shape, which is reused by readyForMariaDB too.
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			// Pull the MageStore back so later in-test mutations run
			// against the server-populated object (defaults applied,
			// resourceVersion set).
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			store.Spec.Database = &magev1alpha1.DatabaseSpec{
				External: &magev1alpha1.ExternalDatabase{
					Host: "mysql.example.com",
					CredentialsSecretRef: magev1alpha1.LocalObjectReference{
						Name: "dbcreds",
					},
				},
			}
		})

		It("reports CredentialSecretMissing when the Secret does not exist", func() {
			cond, endpoint, err := reconciler.ensureDatabase(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionDatabaseReady))
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("CredentialSecretMissing"))
			Expect(endpoint).To(Equal(DatabaseEndpoint{}))
		})

		It("reports CredentialKeyEmpty when a required key is missing", func() {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "dbcreds", Namespace: namespace},
				StringData: map[string]string{"password": "hunter2"}, // no username
			})).To(Succeed())

			cond, endpoint, err := reconciler.ensureDatabase(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("CredentialKeyEmpty"))
			Expect(endpoint).To(Equal(DatabaseEndpoint{}))
		})

		It("resolves the endpoint when the Secret is complete", func() {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "dbcreds", Namespace: namespace},
				StringData: map[string]string{
					"username": "magento",
					"password": "hunter2",
				},
			})).To(Succeed())

			cond, endpoint, err := reconciler.ensureDatabase(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("ExternalReady"))
			Expect(endpoint).To(Equal(DatabaseEndpoint{
				Host:     "mysql.example.com",
				Port:     3306,
				Name:     "magento",
				Username: "magento",
				Password: "hunter2",
			}))
		})
	})

	Context("inline mode", func() {
		// Inline mode owns a MariaDB CR and funnels into the shared
		// readyForMariaDB path after materialisation. Full coverage
		// is: CR is created on first reconcile, Ready stays False
		// until the upstream operator would mark the MariaDB Ready,
		// then flips True once both the status condition and the
		// root-password Secret are in place. We fake the upstream
		// operator by setting the status conditions and creating the
		// Secret ourselves.
		BeforeEach(func() {
			store.Spec.Database = &magev1alpha1.DatabaseSpec{
				MariaDb: &magev1alpha1.InlineMariaDB{
					Version:     "11.4",
					StorageSize: "1Gi",
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("materialises the MariaDB CR and flips Ready once the upstream status + Secret appear", func() {
			// First reconcile: CR does not exist yet, ensureDatabase
			// creates it. Readiness is False because the fake apiserver
			// has not populated any MariaDB.Status.Conditions.
			cond, _, err := reconciler.ensureDatabase(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).NotTo(Equal(metav1.ConditionTrue))

			mdKey := types.NamespacedName{Name: "store-mariadb", Namespace: namespace}
			var md mariadbv1alpha1.MariaDB
			Expect(k8sClient.Get(ctx, mdKey, &md)).To(Succeed())
			Expect(md.Spec.RootPasswordSecretKeyRef.Name).To(Equal("store-mariadb-root"))
			Expect(md.Spec.RootPasswordSecretKeyRef.Key).To(Equal("root-password"))
			Expect(md.Spec.RootPasswordSecretKeyRef.Generate).To(BeTrue())

			// Simulate mariadb-operator reaching Ready: set the status
			// condition and create the root-password Secret it would
			// have generated.
			md.Status.Conditions = []metav1.Condition{{
				Type:               mariadbv1alpha1.ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             "StatefulSetReady",
				Message:            "MariaDB is ready",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, &md)).To(Succeed())

			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "store-mariadb-root", Namespace: namespace},
				StringData: map[string]string{"root-password": "s3cret"},
			})).To(Succeed())

			cond, endpoint, err := reconciler.ensureDatabase(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("MariaDBReady"))
			Expect(endpoint).To(Equal(DatabaseEndpoint{
				Host:     "store-mariadb",
				Port:     3306,
				Name:     "magento",
				Username: "root",
				Password: "s3cret",
			}))
		})
	})

	Context("ref mode", func() {
		// Ref mode shares readyForMariaDB with inline — the difference
		// is only who owns the MariaDB CR. The key test here is the
		// NotFound → Ready transition: before the admin creates the
		// CR we must report False(NotFound); once created and marked
		// Ready we must resolve the endpoint against the pre-existing
		// root-password Secret.
		BeforeEach(func() {
			store.Spec.Database = &magev1alpha1.DatabaseSpec{
				MariaDbRef: &magev1alpha1.LocalObjectReference{Name: "shared-db"},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("reports NotFound until the referenced MariaDB CR exists", func() {
			cond, endpoint, err := reconciler.ensureDatabase(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NotFound"))
			Expect(endpoint).To(Equal(DatabaseEndpoint{}))
		})

		It("resolves the endpoint via the referenced CR's own rootPasswordSecretKeyRef", func() {
			md := &mariadbv1alpha1.MariaDB{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-db", Namespace: namespace},
				Spec: mariadbv1alpha1.MariaDBSpec{
					RootPasswordSecretKeyRef: mariadbv1alpha1.GeneratedSecretKeyRef{
						SecretKeySelector: mariadbv1alpha1.SecretKeySelector{
							LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: "shared-db-root"},
							Key:                  "root-password",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, md)).To(Succeed())
			meta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
				Type:               mariadbv1alpha1.ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             "StatefulSetReady",
				Message:            "MariaDB is ready",
				LastTransitionTime: metav1.Now(),
			})
			Expect(k8sClient.Status().Update(ctx, md)).To(Succeed())

			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-db-root", Namespace: namespace},
				StringData: map[string]string{"root-password": "admin-owned-pw"},
			})).To(Succeed())

			cond, endpoint, err := reconciler.ensureDatabase(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(endpoint.Host).To(Equal("shared-db"))
			Expect(endpoint.Password).To(Equal("admin-owned-pw"))
		})
	})
})
