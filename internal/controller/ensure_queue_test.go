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
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	rabbitmqv1beta1 "github.com/rabbitmq/cluster-operator/v2/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// ensureQueue drives the three discriminated-union modes of spec.queue
// (inline / ref / external). External mode's credentials Secret is
// required (not optional like spec.cache.external) because RabbitMQ
// always authenticates. Inline and ref share readyForRabbitmqCluster
// which walks Status.Conditions — test constructs those via JSON
// unmarshal because the concrete RabbitmqClusterCondition type lives
// in rabbitmq-cluster-operator's `internal/status` package, which
// Go's internal-package rule prohibits us from importing by name. JSON
// unmarshal into the Conditions slice field bypasses the import
// restriction cleanly; the runtime type comes from the exported
// Status.Conditions field's declared type.
var _ = Describe("ensureQueue", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	// Fresh namespace per test — same GenerateName pattern as the
	// ensureCache / ensureDatabase suites. envtest has no namespace
	// controller; Delete would mark namespaces Terminating forever.
	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensurequeue-"},
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
		// External mode always requires credentials — RabbitMQ has no
		// no-auth shape. Three cases: Secret missing, key missing, and
		// full success. Mirrors the Slot-17 Secret-validation pathway
		// with username + password both mandatory.
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("reports CredentialSecretMissing when credentialsSecretRef points at a missing Secret", func() {
			store.Spec.Queue = &magev1alpha1.QueueSpec{
				External: &magev1alpha1.ExternalQueue{
					Host: "rabbit.example.com",
					CredentialsSecretRef: magev1alpha1.LocalObjectReference{
						Name: "queuecreds",
					},
				},
			}

			cond, endpoint, err := reconciler.ensureQueue(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionQueueReady))
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("CredentialSecretMissing"))
			Expect(endpoint).To(Equal(QueueEndpoint{}))
		})

		It("reports CredentialKeyEmpty when the Secret lacks the password key", func() {
			store.Spec.Queue = &magev1alpha1.QueueSpec{
				External: &magev1alpha1.ExternalQueue{
					Host: "rabbit.example.com",
					CredentialsSecretRef: magev1alpha1.LocalObjectReference{
						Name: "queuecreds",
					},
				},
			}
			// Secret with username but no password — half-populated
			// credentials should fail loud, not silently emit empty
			// MAGENTO_RABBITMQ_PASSWORD.
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "queuecreds", Namespace: namespace},
				StringData: map[string]string{"username": "magento"},
			})).To(Succeed())

			cond, endpoint, err := reconciler.ensureQueue(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("CredentialKeyEmpty"))
			Expect(endpoint).To(Equal(QueueEndpoint{}))
		})

		It("resolves the endpoint with full credentials and defaults", func() {
			store.Spec.Queue = &magev1alpha1.QueueSpec{
				External: &magev1alpha1.ExternalQueue{
					Host: "rabbit.example.com",
					Port: 5673,
					CredentialsSecretRef: magev1alpha1.LocalObjectReference{
						Name: "queuecreds",
					},
				},
			}
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "queuecreds", Namespace: namespace},
				StringData: map[string]string{
					"username": "magento",
					"password": "hunter2",
				},
			})).To(Succeed())

			cond, endpoint, err := reconciler.ensureQueue(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("ExternalReady"))
			Expect(endpoint).To(Equal(QueueEndpoint{
				Host:     "rabbit.example.com",
				Port:     5673,
				Vhost:    "/",
				Username: "magento",
				Password: "hunter2",
			}))
		})
	})

	Context("inline mode", func() {
		// Inline mode owns a RabbitmqCluster CR and funnels into the
		// shared readyForRabbitmqCluster path after materialisation.
		// Coverage: CR is created on first reconcile, Ready stays False
		// until both Conditions reach True AND Status.DefaultUser.
		// SecretReference is populated; then flips True. We fake the
		// upstream operator by setting Status via raw JSON (see
		// file-level comment) and creating the default-user Secret it
		// would have generated.
		BeforeEach(func() {
			store.Spec.Queue = &magev1alpha1.QueueSpec{
				RabbitMq: &magev1alpha1.InlineRabbitMQ{},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("materialises the RabbitmqCluster CR and flips Ready once Conditions + SecretReference are in place", func() {
			// First reconcile: CR does not exist yet, ensureQueue
			// creates it. Readiness is False because the fake
			// apiserver has not populated Status.
			cond, _, err := reconciler.ensureQueue(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).NotTo(Equal(metav1.ConditionTrue))

			rcKey := types.NamespacedName{Name: "store-rabbitmq", Namespace: namespace}
			var rc rabbitmqv1beta1.RabbitmqCluster
			Expect(k8sClient.Get(ctx, rcKey, &rc)).To(Succeed())
			Expect(rc.Spec.Replicas).NotTo(BeNil())
			Expect(*rc.Spec.Replicas).To(Equal(int32(1)))

			// Simulate upstream reaching Ready: populate the two
			// Conditions we gate on (ClusterAvailable +
			// AllReplicasReady) via JSON unmarshal, plant the
			// default-user Secret, and set the self-describing
			// SecretReference so our reader path matches production.
			Expect(json.Unmarshal([]byte(`[
				{"type":"ClusterAvailable","status":"True","reason":"Ready"},
				{"type":"AllReplicasReady","status":"True","reason":"Ready"}
			]`), &rc.Status.Conditions)).To(Succeed())
			rc.Status.DefaultUser = &rabbitmqv1beta1.RabbitmqClusterDefaultUser{
				SecretReference: &rabbitmqv1beta1.RabbitmqClusterSecretReference{
					Name:      "store-rabbitmq-default-user",
					Namespace: namespace,
					Keys: map[string]string{
						"username": "username",
						"password": "password",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, &rc)).To(Succeed())

			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "store-rabbitmq-default-user", Namespace: namespace},
				StringData: map[string]string{
					"username": "auto-gen-user",
					"password": "auto-gen-pw",
				},
			})).To(Succeed())

			cond, endpoint, err := reconciler.ensureQueue(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("RabbitmqClusterReady"))
			Expect(endpoint).To(Equal(QueueEndpoint{
				Host:     "store-rabbitmq",
				Port:     5672,
				Vhost:    "/",
				Username: "auto-gen-user",
				Password: "auto-gen-pw",
			}))
		})

		It("reports NotReady when only ClusterAvailable is True but AllReplicasReady is not", func() {
			// First reconcile creates the CR.
			_, _, err := reconciler.ensureQueue(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			rcKey := types.NamespacedName{Name: "store-rabbitmq", Namespace: namespace}
			var rc rabbitmqv1beta1.RabbitmqCluster
			Expect(k8sClient.Get(ctx, rcKey, &rc)).To(Succeed())

			// Only ClusterAvailable is True — Slot 20's readiness
			// requires both conditions to ensure new replicas have
			// actually settled (not just "the cluster can serve").
			Expect(json.Unmarshal([]byte(`[
				{"type":"ClusterAvailable","status":"True","reason":"Ready"},
				{"type":"AllReplicasReady","status":"False","reason":"NotAllPodsReady"}
			]`), &rc.Status.Conditions)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, &rc)).To(Succeed())

			cond, endpoint, err := reconciler.ensureQueue(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NotReady"))
			Expect(endpoint).To(Equal(QueueEndpoint{}))
		})
	})

	Context("ref mode", func() {
		// Ref mode shares readyForRabbitmqCluster with inline. NotFound
		// → Ready transition plus endpoint resolution against an
		// admin-created RabbitmqCluster CR + its default-user Secret is
		// the minimum useful coverage.
		BeforeEach(func() {
			store.Spec.Queue = &magev1alpha1.QueueSpec{
				RabbitMqRef: &magev1alpha1.LocalObjectReference{Name: "shared-broker"},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("reports NotFound until the referenced RabbitmqCluster exists", func() {
			cond, endpoint, err := reconciler.ensureQueue(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NotFound"))
			Expect(endpoint).To(Equal(QueueEndpoint{}))
		})

		It("resolves the endpoint via the admin-managed RabbitmqCluster's default-user Secret", func() {
			rc := &rabbitmqv1beta1.RabbitmqCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-broker", Namespace: namespace},
			}
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			Expect(json.Unmarshal([]byte(`[
				{"type":"ClusterAvailable","status":"True","reason":"Ready"},
				{"type":"AllReplicasReady","status":"True","reason":"Ready"}
			]`), &rc.Status.Conditions)).To(Succeed())
			rc.Status.DefaultUser = &rabbitmqv1beta1.RabbitmqClusterDefaultUser{
				SecretReference: &rabbitmqv1beta1.RabbitmqClusterSecretReference{
					Name:      "shared-broker-default-user",
					Namespace: namespace,
					Keys: map[string]string{
						"username": "username",
						"password": "password",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, rc)).To(Succeed())

			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-broker-default-user", Namespace: namespace},
				StringData: map[string]string{
					"username": "admin-owned",
					"password": "admin-pw",
				},
			})).To(Succeed())

			cond, endpoint, err := reconciler.ensureQueue(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(endpoint.Host).To(Equal("shared-broker"))
			Expect(endpoint.Port).To(Equal(int32(5672)))
			Expect(endpoint.Vhost).To(Equal("/"))
			Expect(endpoint.Username).To(Equal("admin-owned"))
			Expect(endpoint.Password).To(Equal("admin-pw"))
		})
	})
})
