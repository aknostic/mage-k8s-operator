/*
Copyright 2026.
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// CRD-shape roundtrip suite for spec.upgrade. No reconciler behaviour
// is exercised here — the assertions verify the API server preserves /
// defaults the new fields per Subsession A's surface (ADR 008).
//
// The tests live alongside controller envtests because the suite-level
// envtest already loads config/crd/bases/...yaml; api/v1alpha1 has no
// own envtest harness as of v0.1.2.
var _ = Describe("MageStore spec.upgrade CRD shape", func() {
	var namespace string

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "upgrade-shape-"},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name
	})

	baseSpec := func() magev1alpha1.MageStoreSpec {
		return magev1alpha1.MageStoreSpec{
			Image: magev1alpha1.MageStoreImage{
				Repository: "registry.gitlab.aknostic.com/test/magento",
				Tag:        "2.4.8",
			},
		}
	}

	It("roundtrips with spec.upgrade == nil when omitted", func() {
		store := &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: namespace},
			Spec:       baseSpec(),
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())

		got := &magev1alpha1.MageStore{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, got)).To(Succeed())
		Expect(got.Spec.Upgrade).To(BeNil())
	})

	It("preserves explicit spec.upgrade.maintenance.enabled=false", func() {
		falseVal := false
		spec := baseSpec()
		spec.Upgrade = &magev1alpha1.MageStoreUpgradeSpec{
			Maintenance: &magev1alpha1.MageStoreUpgradeMaintenanceSpec{
				Enabled: &falseVal,
			},
		}
		store := &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: namespace},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())

		got := &magev1alpha1.MageStore{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, got)).To(Succeed())
		Expect(got.Spec.Upgrade).NotTo(BeNil())
		Expect(got.Spec.Upgrade.Maintenance).NotTo(BeNil())
		Expect(got.Spec.Upgrade.Maintenance.Enabled).NotTo(BeNil())
		Expect(*got.Spec.Upgrade.Maintenance.Enabled).To(BeFalse())
	})

	It("defaults spec.upgrade.maintenance.enabled to true when maintenance is set without enabled", func() {
		spec := baseSpec()
		spec.Upgrade = &magev1alpha1.MageStoreUpgradeSpec{
			Maintenance: &magev1alpha1.MageStoreUpgradeMaintenanceSpec{},
		}
		store := &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: namespace},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())

		got := &magev1alpha1.MageStore{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, got)).To(Succeed())
		Expect(got.Spec.Upgrade).NotTo(BeNil())
		Expect(got.Spec.Upgrade.Maintenance).NotTo(BeNil())
		Expect(got.Spec.Upgrade.Maintenance.Enabled).NotTo(BeNil())
		Expect(*got.Spec.Upgrade.Maintenance.Enabled).To(BeTrue())
	})

	It("defaults htmlConfigMapRef.key to index.html when name is set without key", func() {
		spec := baseSpec()
		spec.Upgrade = &magev1alpha1.MageStoreUpgradeSpec{
			Maintenance: &magev1alpha1.MageStoreUpgradeMaintenanceSpec{
				HtmlConfigMapRef: &magev1alpha1.MaintenanceHtmlConfigMapRef{
					Name: "my-503",
				},
			},
		}
		store := &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: namespace},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())

		got := &magev1alpha1.MageStore{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, got)).To(Succeed())
		Expect(got.Spec.Upgrade).NotTo(BeNil())
		Expect(got.Spec.Upgrade.Maintenance).NotTo(BeNil())
		Expect(got.Spec.Upgrade.Maintenance.HtmlConfigMapRef).NotTo(BeNil())
		Expect(got.Spec.Upgrade.Maintenance.HtmlConfigMapRef.Name).To(Equal("my-503"))
		Expect(got.Spec.Upgrade.Maintenance.HtmlConfigMapRef.Key).To(Equal("index.html"))
	})
})
