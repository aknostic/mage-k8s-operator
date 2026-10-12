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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// ensureCache covers the four post-ADR-009 modes:
//
//   - external: unmanaged endpoint, Secret-only readiness.
//   - inline single-instance (`replicas: 1`): operator-owned bare
//     Valkey StatefulSet + headless Service + auto-generated password
//     Secret + ConfigMap.
//   - inline HA (`replicas: 3`): adds Sentinel ConfigMap, ClusterIP
//     Service, and Deployment on top of the single-instance shape.
//   - redisRef: operator-agnostic — points at any Service in the
//     namespace; password from a conventional `<service-name>` Secret.
//
// The orphan sweep (gap #2b) is exercised by transitioning
// `spec.cache` from inline to nil and verifying the StatefulSet,
// Services, ConfigMap, and password Secret are deleted.
var _ = Describe("ensureCache", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	// Fresh namespace per test — same GenerateName pattern as the
	// ensureDatabase suite (Slot 18). envtest has no namespace
	// controller; Delete would mark namespaces Terminating forever
	// and cause name reuse to collide.
	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensurecache-"},
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
					Repository: "registry.gitlab.aknostic.com/test/magento",
					Tag:        "2.4.8",
				},
			},
		}
	})

	Context("external mode", func() {
		// External mode splits into two shapes: no PasswordSecretRef
		// (password-less endpoint, Ready on first reconcile) and
		// PasswordSecretRef set (Secret-validation pathway). Both
		// preserved unchanged across the ADR-009 reshape.
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("resolves a password-less external endpoint with no auth env var", func() {
			store.Spec.Cache = &magev1alpha1.CacheSpec{
				External: &magev1alpha1.ExternalCache{
					Host: "redis.example.com",
				},
			}

			cond, endpoint, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionCacheReady))
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("ExternalReady"))
			Expect(endpoint).To(Equal(CacheEndpoint{
				Host: "redis.example.com",
				Port: 6379,
			}))
		})

		It("reports CredentialSecretMissing when PasswordSecretRef points at a missing Secret", func() {
			store.Spec.Cache = &magev1alpha1.CacheSpec{
				External: &magev1alpha1.ExternalCache{
					Host: "redis.example.com",
					Port: 6380,
					PasswordSecretRef: &magev1alpha1.SecretKeySelector{
						Name: "cachecreds",
					},
				},
			}

			cond, endpoint, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("CredentialSecretMissing"))
			Expect(endpoint).To(Equal(CacheEndpoint{}))
		})

		It("resolves the endpoint with AUTH when the password Secret is complete", func() {
			store.Spec.Cache = &magev1alpha1.CacheSpec{
				External: &magev1alpha1.ExternalCache{
					Host: "redis.example.com",
					PasswordSecretRef: &magev1alpha1.SecretKeySelector{
						Name: "cachecreds",
					},
				},
			}
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "cachecreds", Namespace: namespace},
				Data:       map[string][]byte{"password": []byte("hunter2")},
			})).To(Succeed())

			cond, endpoint, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("ExternalReady"))
			Expect(endpoint).To(Equal(CacheEndpoint{
				Host:     "redis.example.com",
				Port:     6379,
				Password: "hunter2",
			}))
		})
	})

	Context("inline mode — single-instance", func() {
		// Single-instance: bare StatefulSet (1 replica) + headless
		// Service + ConfigMap (valkey.conf, bootstrap.sh) + auto-
		// generated password Secret. Coverage: first reconcile
		// materialises everything but Ready stays False until the
		// StatefulSet's status reports a ready replica; once we fake
		// that status, Ready flips True with the headless Service host.
		BeforeEach(func() {
			store.Spec.Cache = &magev1alpha1.CacheSpec{
				Redis: &magev1alpha1.InlineRedis{Replicas: 1},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("materialises StatefulSet + Service + ConfigMap + Secret on first reconcile", func() {
			cond, endpoint, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NotReady"))
			Expect(endpoint).To(Equal(CacheEndpoint{}))

			ss := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey", Namespace: namespace}, ss)).To(Succeed())
			Expect(*ss.Spec.Replicas).To(Equal(int32(1)))
			Expect(ss.Spec.ServiceName).To(Equal("store-valkey"))
			Expect(ss.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(ss.Spec.Template.Spec.Containers[0].Image).To(Equal(valkeyImage))
			Expect(ss.Spec.VolumeClaimTemplates).To(HaveLen(1))

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey", Namespace: namespace}, svc)).To(Succeed())
			Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-config", Namespace: namespace}, cm)).To(Succeed())
			Expect(cm.Data).To(HaveKey("valkey.conf"))
			Expect(cm.Data).To(HaveKey("bootstrap.sh"))

			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-auth", Namespace: namespace}, sec)).To(Succeed())
			Expect(sec.Data).To(HaveKey("password"))
			Expect(sec.Data["password"]).NotTo(BeEmpty())
		})

		It("flips Ready=True once the StatefulSet reports a ready replica", func() {
			_, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			ss := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey", Namespace: namespace}, ss)).To(Succeed())
			ss.Status.Replicas = 1
			ss.Status.ReadyReplicas = 1
			ss.Status.AvailableReplicas = 1
			Expect(k8sClient.Status().Update(ctx, ss)).To(Succeed())

			cond, endpoint, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("ValkeyReady"))
			Expect(endpoint.Host).To(Equal("store-valkey"))
			Expect(endpoint.Port).To(Equal(int32(6379)))
			Expect(endpoint.Password).NotTo(BeEmpty())
			Expect(endpoint.SentinelMaster).To(BeEmpty())
		})

		It("preserves the auto-generated password across reconciles", func() {
			_, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-auth", Namespace: namespace}, sec)).To(Succeed())
			first := string(sec.Data["password"])

			_, _, err = reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-auth", Namespace: namespace}, sec)).To(Succeed())
			Expect(string(sec.Data["password"])).To(Equal(first))
		})

		It("renders bootstrap.sh with the Sentinel query short-circuited (empty SENTINEL_HOST)", func() {
			// Single-instance opt-out: an empty SENTINEL_HOST in the
			// bootstrap script makes discover_master() return 1
			// immediately, so the script falls through to ordinal-only
			// logic and never tries to talk to a Sentinel that doesn't
			// exist in this topology.
			_, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-config", Namespace: namespace}, cm)).To(Succeed())
			Expect(cm.Data["bootstrap.sh"]).To(ContainSubstring("SENTINEL_HOST=''"))
			Expect(cm.Data["bootstrap.sh"]).NotTo(ContainSubstring("SENTINEL_HOST='store-valkey-sentinel'"))
			// Ordinal-only fallback line must still be present so a
			// hypothetical scale-out still uses pod-0 as the seed master.
			Expect(cm.Data["bootstrap.sh"]).To(ContainSubstring("--replicaof store-valkey-0.store-valkey 6379"))
		})
	})

	Context("inline mode — Sentinel HA", func() {
		// HA mode adds Sentinel ConfigMap, ClusterIP Service, and
		// Deployment on top of the single-instance topology. Coverage:
		// (1) all four artifacts materialise with the right replica
		// count and quorum; (2) Ready flips True only once both the
		// StatefulSet and Sentinel Deployment reach quorum.
		BeforeEach(func() {
			store.Spec.Cache = &magev1alpha1.CacheSpec{
				Redis: &magev1alpha1.InlineRedis{Replicas: 3},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("materialises StatefulSet, Sentinel Deployment, and Sentinel Service with quorum=2", func() {
			_, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			ss := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey", Namespace: namespace}, ss)).To(Succeed())
			Expect(*ss.Spec.Replicas).To(Equal(int32(3)))

			sentinel := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-sentinel", Namespace: namespace}, sentinel)).To(Succeed())
			Expect(*sentinel.Spec.Replicas).To(Equal(int32(3)))

			sentinelSvc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-sentinel", Namespace: namespace}, sentinelSvc)).To(Succeed())
			Expect(sentinelSvc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(sentinelSvc.Spec.Ports[0].Port).To(Equal(sentinelPort))

			sentinelCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-sentinel-config", Namespace: namespace}, sentinelCM)).To(Succeed())
			// Quorum (3/2)+1 = 2 must appear in the monitor line; the
			// password placeholder must NOT be substituted at the
			// ConfigMap layer (substitution happens at runtime via the
			// bootstrap script).
			Expect(sentinelCM.Data["sentinel.conf"]).To(ContainSubstring("monitor mymaster store-valkey-0.store-valkey 6379 2"))
			Expect(sentinelCM.Data["sentinel.conf"]).To(ContainSubstring("__PASSWORD__"))
		})

		It("renders bootstrap.sh with the Sentinel-query stanza pointing at the Sentinel ClusterIP", func() {
			// HA mode: the bootstrap script queries Sentinel before
			// defaulting to ordinal logic so a restart after a
			// Sentinel-driven failover joins the new master as a
			// replica instead of reverting to ordinal-master role.
			_, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-config", Namespace: namespace}, cm)).To(Succeed())
			script := cm.Data["bootstrap.sh"]
			Expect(script).To(ContainSubstring("SENTINEL_HOST='store-valkey-sentinel'"))
			Expect(script).To(ContainSubstring("SENTINEL get-master-addr-by-name mymaster"))
			Expect(script).To(ContainSubstring("timeout 3 valkey-cli"))
			// Ordinal-only fallback (cold-start before Sentinel reaches
			// quorum) must remain.
			Expect(script).To(ContainSubstring("--replicaof store-valkey-0.store-valkey 6379"))
		})

		It("flips Ready=True only once StatefulSet AND Sentinel reach quorum", func() {
			_, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			// First: StatefulSet alone is ready, Sentinel still pending.
			// status.replicas must be set before the API server allows
			// availableReplicas > 0 — its validator rejects
			// availableReplicas > status.replicas with a 422.
			ss := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey", Namespace: namespace}, ss)).To(Succeed())
			ss.Status.Replicas = 3
			ss.Status.ReadyReplicas = 1
			ss.Status.AvailableReplicas = 1
			Expect(k8sClient.Status().Update(ctx, ss)).To(Succeed())

			cond, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("SentinelBelowQuorum"))

			// Now bring Sentinel to quorum (2/3 available).
			sentinel := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-sentinel", Namespace: namespace}, sentinel)).To(Succeed())
			sentinel.Status.Replicas = 3
			sentinel.Status.AvailableReplicas = 2
			sentinel.Status.ReadyReplicas = 2
			Expect(k8sClient.Status().Update(ctx, sentinel)).To(Succeed())

			cond, endpoint, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("SentinelReady"))
			Expect(endpoint.Host).To(Equal("store-valkey-sentinel"))
			Expect(endpoint.Port).To(Equal(sentinelPort))
			Expect(endpoint.SentinelMaster).To(Equal("mymaster"))
		})
	})

	Context("redisRef mode — operator-agnostic Service pointer", func() {
		// Post-ADR-009 redisRef points at a Service (any third-party
		// Redis/Valkey operator's front-door, or a hand-rolled
		// deployment), not at a hyperspike Valkey CR. Password resolves
		// from a conventional `<service-name>` Secret.
		BeforeEach(func() {
			store.Spec.Cache = &magev1alpha1.CacheSpec{
				RedisRef: &magev1alpha1.LocalObjectReference{Name: "shared-cache"},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("reports NotFound until the referenced Service exists", func() {
			cond, endpoint, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NotFound"))
			Expect(endpoint).To(Equal(CacheEndpoint{}))
		})

		It("resolves the endpoint via the Service + conventional `<service-name>` password Secret", func() {
			Expect(k8sClient.Create(ctx, &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-cache", Namespace: namespace},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Port: 6379}},
				},
			})).To(Succeed())
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-cache", Namespace: namespace},
				Data:       map[string][]byte{"password": []byte("admin-owned-pw")},
			})).To(Succeed())

			cond, endpoint, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("ServiceReady"))
			Expect(endpoint.Host).To(Equal("shared-cache"))
			Expect(endpoint.Port).To(Equal(int32(6379)))
			Expect(endpoint.Password).To(Equal("admin-owned-pw"))
			Expect(endpoint.SentinelMaster).To(BeEmpty())
		})
	})

	Context("cross-topology resize opt-in — v0.4.0", func() {
		// Cross-topology cache resize (replicas: 1 ↔ 3+) discards
		// cache contents because Sentinel HA's data isn't migratable
		// to a single-instance shape and vice versa. The operator
		// refuses such transitions without the cache-topology-change
		// annotation set to "approved" on the MageStore. With the
		// annotation, it deletes the existing cache resources and
		// reports TopologyTransitioning until the next reconcile
		// materialises the new topology fresh.
		//
		// Within-topology resize (e.g. 3 → 5 in HA) is online and
		// proceeds without requiring approval.

		It("allows within-topology resize 3 → 5 without an annotation", func() {
			// First reconcile at replicas=3.
			store.Spec.Cache = &magev1alpha1.CacheSpec{
				Redis: &magev1alpha1.InlineRedis{Replicas: 3},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			_, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			// Bump to replicas=5; topology stays HA.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Spec.Cache.Redis.Replicas = 5
			cond, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Reason).NotTo(Equal(ReasonTopologyChangeRequiresApproval))
			Expect(cond.Reason).NotTo(Equal(ReasonTopologyTransitioning))

			ss := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey", Namespace: namespace}, ss)).To(Succeed())
			Expect(*ss.Spec.Replicas).To(Equal(int32(5)))
		})

		It("refuses cross-topology resize 1 → 3 when the annotation is absent", func() {
			// First reconcile at replicas=1.
			store.Spec.Cache = &magev1alpha1.CacheSpec{
				Redis: &magev1alpha1.InlineRedis{Replicas: 1},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			_, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			// Bump to replicas=3 without the annotation; topology
			// changes single → ha and must refuse.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Spec.Cache.Redis.Replicas = 3
			cond, endpoint, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonTopologyChangeRequiresApproval))
			Expect(cond.Message).To(ContainSubstring(cacheTopologyChangeAnnotation))
			Expect(cond.Message).To(ContainSubstring(cacheTopologyChangeApproved))
			Expect(endpoint).To(Equal(CacheEndpoint{}))

			// SS must still exist at the OLD topology (untouched).
			ss := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey", Namespace: namespace}, ss)).To(Succeed())
			Expect(*ss.Spec.Replicas).To(Equal(int32(1)))

			// No Sentinel artifacts for the new topology should have
			// been created — the refusal is supposed to short-circuit.
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-sentinel", Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("executes cross-topology resize 3 → 1 when the annotation is approved, and clears the annotation", func() {
			// First reconcile at replicas=3 (HA).
			store.Spec.Cache = &magev1alpha1.CacheSpec{
				Redis: &magev1alpha1.InlineRedis{Replicas: 3},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			_, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			// Confirm Sentinel artifacts materialised (we'll assert
			// they get swept on the transition).
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-sentinel", Namespace: namespace}, &appsv1.Deployment{})).To(Succeed())

			// Approve the topology change and switch to replicas=1.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			if store.Annotations == nil {
				store.Annotations = map[string]string{}
			}
			store.Annotations[cacheTopologyChangeAnnotation] = cacheTopologyChangeApproved
			Expect(k8sClient.Update(ctx, store)).To(Succeed())
			store.Spec.Cache.Redis.Replicas = 1
			Expect(k8sClient.Update(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			cond, endpoint, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonTopologyTransitioning))
			Expect(endpoint).To(Equal(CacheEndpoint{}))

			// StatefulSet was deleted (or marked for deletion in
			// envtest's no-real-GC environment).
			ss := &appsv1.StatefulSet{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey", Namespace: namespace}, ss)
			if err == nil {
				Expect(ss.DeletionTimestamp).NotTo(BeNil())
			} else {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}

			// Sentinel artifacts swept.
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-sentinel", Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-sentinel", Namespace: namespace}, &corev1.Service{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			// Annotation cleared from the MageStore so a subsequent
			// reconcile/edit needs fresh approval.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			Expect(store.Annotations).NotTo(HaveKey(cacheTopologyChangeAnnotation))
		})
	})

	Context("orphan sweep — gap #2b", func() {
		// Removing `spec.cache` after a previous inline reconcile must
		// delete every operator-owned cache resource in the namespace
		// — StatefulSet, Sentinel Deployment (if any), Services,
		// ConfigMaps, and the auto-generated password Secret.
		It("deletes inline-mode cache resources when spec.cache transitions to nil", func() {
			store.Spec.Cache = &magev1alpha1.CacheSpec{
				Redis: &magev1alpha1.InlineRedis{Replicas: 3},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			// Confirm the cache resources exist.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey", Namespace: namespace}, &appsv1.StatefulSet{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-sentinel", Namespace: namespace}, &appsv1.Deployment{})).To(Succeed())

			// Transition to nil and reconcile.
			store.Spec.Cache = nil
			cond, _, err := reconciler.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("NotConfigured"))

			// Every owned resource is gone.
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey", Namespace: namespace}, &appsv1.StatefulSet{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-sentinel", Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey", Namespace: namespace}, &corev1.Service{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-sentinel", Namespace: namespace}, &corev1.Service{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-config", Namespace: namespace}, &corev1.ConfigMap{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-valkey-auth", Namespace: namespace}, &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

	})
})
