/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// ensureMedia covers the four volume modes (unset = emptyDir back-compat,
// explicit emptyDir, pvc = operator-provisioned PVC, s3 = emptyDir scratch +
// env vars) and the orthogonal SeedFromImage flag.
// No condition to test — the function returns MediaEndpoint + err.
var _ = Describe("ensureMedia", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ensuremedia-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		reconciler = &MageStoreReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Capabilities: fullCapabilities()}
		store = &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: namespace},
			Spec: magev1alpha1.MageStoreSpec{
				Image: magev1alpha1.MageStoreImage{
					Repository: "registry.example.com/test/magento",
					Tag:        "2.4.8",
				},
				Workloads: magev1alpha1.WorkloadsSpec{
					Frontend: magev1alpha1.WorkloadSpec{Replicas: 1},
				},
			},
		}
	})

	It("returns an emptyDir volume when spec.media is nil (back-compat)", func() {
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		endpoint, err := reconciler.ensureMedia(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoint.Volume.Name).To(Equal("media"))
		Expect(endpoint.Volume.EmptyDir).NotTo(BeNil())
		Expect(endpoint.EnvVars).To(BeEmpty())
		Expect(endpoint.SeedFromImage).To(BeFalse())
	})

	It("returns an emptyDir volume in explicit emptyDir mode without seedFromImage", func() {
		store.Spec.Media = &magev1alpha1.MediaSpec{
			EmptyDir: &magev1alpha1.MediaEmptyDir{},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		endpoint, err := reconciler.ensureMedia(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoint.Volume.Name).To(Equal("media"))
		Expect(endpoint.Volume.EmptyDir).NotTo(BeNil())
		Expect(endpoint.EnvVars).To(BeEmpty())
		Expect(endpoint.SeedFromImage).To(BeFalse())
	})

	It("returns SeedFromImage=true in explicit emptyDir mode when seedFromImage is set", func() {
		store.Spec.Media = &magev1alpha1.MediaSpec{
			EmptyDir:      &magev1alpha1.MediaEmptyDir{},
			SeedFromImage: true,
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		endpoint, err := reconciler.ensureMedia(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoint.Volume.EmptyDir).NotTo(BeNil())
		Expect(endpoint.SeedFromImage).To(BeTrue())
	})

	It("provisions a PVC and returns it as the volume source in pvc mode", func() {
		store.Spec.Media = &magev1alpha1.MediaSpec{
			Pvc: &magev1alpha1.MediaPVC{Size: "5Gi"},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		endpoint, err := reconciler.ensureMedia(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoint.Volume.PersistentVolumeClaim).NotTo(BeNil())
		Expect(endpoint.Volume.PersistentVolumeClaim.ClaimName).To(Equal("store-media"))
		Expect(endpoint.EnvVars).To(BeEmpty())
		Expect(endpoint.SeedFromImage).To(BeFalse())

		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-media", Namespace: namespace}, &pvc)).To(Succeed())
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("5Gi"))
	})

	It("returns SeedFromImage=true in pvc mode when seedFromImage is set", func() {
		store.Spec.Media = &magev1alpha1.MediaSpec{
			Pvc:           &magev1alpha1.MediaPVC{Size: "5Gi"},
			SeedFromImage: true,
		}
		store.Name = "store-pvc-seed"
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-pvc-seed", Namespace: namespace}, store)).To(Succeed())

		endpoint, err := reconciler.ensureMedia(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoint.Volume.PersistentVolumeClaim).NotTo(BeNil())
		Expect(endpoint.SeedFromImage).To(BeTrue())
	})

	It("returns MAGENTO_MEDIA_STORAGE_* env vars when s3 credentials Secret is present", func() {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "store-media-creds", Namespace: namespace},
			Data: map[string][]byte{
				"accessKeyId":     []byte("AKIAEXAMPLE"),
				"secretAccessKey": []byte("secretvalue"),
			},
		}
		Expect(k8sClient.Create(ctx, sec)).To(Succeed())

		store.Spec.Media = &magev1alpha1.MediaSpec{
			S3: &magev1alpha1.MediaS3{
				Bucket:               "my-media",
				Region:               "eu-west-1",
				CredentialsSecretRef: magev1alpha1.LocalObjectReference{Name: "store-media-creds"},
			},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		endpoint, err := reconciler.ensureMedia(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoint.Volume.EmptyDir).NotTo(BeNil()) // still an emptyDir scratch
		Expect(endpoint.EnvVars).To(HaveKeyWithValue("MAGENTO_MEDIA_STORAGE_BUCKET", "my-media"))
		Expect(endpoint.EnvVars).To(HaveKeyWithValue("MAGENTO_MEDIA_STORAGE_REGION", "eu-west-1"))
		Expect(endpoint.EnvVars).To(HaveKeyWithValue("MAGENTO_MEDIA_STORAGE_ACCESS_KEY_ID", "AKIAEXAMPLE"))
		Expect(endpoint.EnvVars).To(HaveKeyWithValue("MAGENTO_MEDIA_STORAGE_SECRET_ACCESS_KEY", "secretvalue"))
		Expect(endpoint.EnvVars).NotTo(HaveKey("MAGENTO_MEDIA_STORAGE_ENDPOINT"))
		Expect(endpoint.SeedFromImage).To(BeFalse())
	})

	It("returns SeedFromImage=true in s3 mode when seedFromImage is set", func() {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "store-s3-seed-creds", Namespace: namespace},
			Data: map[string][]byte{
				"accessKeyId":     []byte("AKIAEXAMPLE"),
				"secretAccessKey": []byte("secretvalue"),
			},
		}
		Expect(k8sClient.Create(ctx, sec)).To(Succeed())

		store.Name = "store-s3-seed"
		store.Spec.Media = &magev1alpha1.MediaSpec{
			S3: &magev1alpha1.MediaS3{
				Bucket:               "my-media",
				Region:               "eu-west-1",
				CredentialsSecretRef: magev1alpha1.LocalObjectReference{Name: "store-s3-seed-creds"},
			},
			SeedFromImage: true,
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-s3-seed", Namespace: namespace}, store)).To(Succeed())

		endpoint, err := reconciler.ensureMedia(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoint.Volume.EmptyDir).NotTo(BeNil())
		Expect(endpoint.EnvVars).To(HaveKey("MAGENTO_MEDIA_STORAGE_BUCKET"))
		Expect(endpoint.SeedFromImage).To(BeTrue())
	})

	It("returns an error when the s3 credentials Secret is missing", func() {
		store.Spec.Media = &magev1alpha1.MediaSpec{
			S3: &magev1alpha1.MediaS3{
				Bucket:               "my-media",
				Region:               "eu-west-1",
				CredentialsSecretRef: magev1alpha1.LocalObjectReference{Name: "missing"},
			},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		_, err := reconciler.ensureMedia(ctx, store)
		Expect(err).To(HaveOccurred())
	})
})
