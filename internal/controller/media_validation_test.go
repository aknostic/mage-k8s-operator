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

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

var _ = Describe("MageStore CRD validation — spec.media", func() {
	const namespace = "default"
	ctx := context.Background()

	It("rejects spec.media: {} (no discriminator set)", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "media-empty"
		store.Spec.Media = &magev1alpha1.MediaSpec{}
		err := k8sClient.Create(ctx, store)
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring("exactly one of emptyDir, pvc, or s3 must be set")))
	})

	It("rejects spec.media with both emptyDir and pvc set", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "media-both-emptydir-pvc"
		store.Spec.Media = &magev1alpha1.MediaSpec{
			EmptyDir: &magev1alpha1.MediaEmptyDir{},
			Pvc:      &magev1alpha1.MediaPVC{Size: "5Gi"},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring("exactly one of emptyDir, pvc, or s3 must be set")))
	})

	It("rejects spec.media with both emptyDir and s3 set", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "media-both-emptydir-s3"
		store.Spec.Media = &magev1alpha1.MediaSpec{
			EmptyDir: &magev1alpha1.MediaEmptyDir{},
			S3: &magev1alpha1.MediaS3{
				Bucket:               "my-bucket",
				Region:               "eu-west-1",
				CredentialsSecretRef: magev1alpha1.LocalObjectReference{Name: "s3-creds"},
			},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring("exactly one of emptyDir, pvc, or s3 must be set")))
	})

	It("accepts spec.media: { emptyDir: {} }", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "media-emptydir-valid"
		store.Spec.Media = &magev1alpha1.MediaSpec{
			EmptyDir: &magev1alpha1.MediaEmptyDir{},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		_ = k8sClient.Delete(ctx, store)
	})

	It("accepts spec.media: { emptyDir: {}, seedFromImage: true }", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "media-emptydir-seed"
		store.Spec.Media = &magev1alpha1.MediaSpec{
			EmptyDir:      &magev1alpha1.MediaEmptyDir{},
			SeedFromImage: true,
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		_ = k8sClient.Delete(ctx, store)
	})

	It("accepts spec.media with only pvc set", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "media-pvc-valid"
		store.Spec.Media = &magev1alpha1.MediaSpec{
			Pvc: &magev1alpha1.MediaPVC{Size: "10Gi"},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		_ = k8sClient.Delete(ctx, store)
	})

	It("accepts spec.media with only s3 set", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "media-s3-valid"
		store.Spec.Media = &magev1alpha1.MediaSpec{
			S3: &magev1alpha1.MediaS3{
				Bucket:               "my-bucket",
				Region:               "eu-west-1",
				CredentialsSecretRef: magev1alpha1.LocalObjectReference{Name: "s3-creds"},
			},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		_ = k8sClient.Delete(ctx, store)
	})
})
