package controller

import (
	"context"
	"fmt"

	apiextclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// Capabilities records which optional dependency CRDs were present in
// the cluster at operator startup. Reconcile-time `ensure*` steps and
// SetupWithManager's `Watches()` chain consult this struct so the
// operator stays usable on a cluster that ships only some of the
// composed operators (e.g. mariadb-operator + envoy-gateway but no
// rabbitmq-cluster-operator). Missing soft deps surface to the user
// via per-feature `*Ready=False, Reason=DependencyOperatorMissing`
// conditions; missing hard deps fail-fast at startup.
//
// Flags reflect a snapshot taken before manager start. CRDs installed
// after startup do NOT cause the operator to gain capabilities until a
// restart — this is the same trade-off controller-runtime makes for
// scheme registration. Document at the prerequisites layer rather than
// papering over with dynamic re-discovery.
type Capabilities struct {
	HasMariaDB    bool
	HasOpenSearch bool
	HasRabbitmq   bool
	HasGateway    bool
	HasHTTPRoute  bool
}

// crdName / dependencyOperator names — kept here so the probe and the
// log/condition messages share a single source of truth. The dependency
// operator label is what users see in `Reason=DependencyOperatorMissing,
// Message=<operator>` and in the startup warning log line.
const (
	crdMariaDB    = "mariadbs.k8s.mariadb.com"
	crdOpenSearch = "opensearchclusters.opensearch.org"
	crdRabbitmq   = "rabbitmqclusters.rabbitmq.com"
	crdGateway    = "gateways.gateway.networking.k8s.io"
	crdHTTPRoute  = "httproutes.gateway.networking.k8s.io"
	depMariaDB    = "mariadb-operator"
	depOpenSearch = "opensearch-k8s-operator"
	depRabbitmq   = "rabbitmq-cluster-operator"
	depGatewayAPI = "gateway-api"
)

// ProbeCapabilities asks the apiserver which dependency CRDs exist and
// returns a Capabilities snapshot. The apiextensions client is built
// from the same rest.Config the manager will use, so a failure here
// indicates the operator can't reach its own apiserver — a fatal
// startup error, not a soft signal.
func ProbeCapabilities(ctx context.Context, cfg *rest.Config) (Capabilities, error) {
	cs, err := apiextclientset.NewForConfig(cfg)
	if err != nil {
		return Capabilities{}, fmt.Errorf("apiextensions clientset: %w", err)
	}

	check := func(name string) (bool, error) {
		_, err := cs.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return true, nil
		}
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get CRD %q: %w", name, err)
	}

	caps := Capabilities{}
	for _, probe := range []struct {
		name string
		set  *bool
	}{
		{crdMariaDB, &caps.HasMariaDB},
		{crdOpenSearch, &caps.HasOpenSearch},
		{crdRabbitmq, &caps.HasRabbitmq},
		{crdGateway, &caps.HasGateway},
		{crdHTTPRoute, &caps.HasHTTPRoute},
	} {
		present, err := check(probe.name)
		if err != nil {
			return Capabilities{}, err
		}
		*probe.set = present
	}
	return caps, nil
}

// dependencyMissingCondition synthesises the per-feature condition
// surfaced when the user opted into an inline/ref mode whose CRD isn't
// installed. The `Type` and `Reason` are the operator-facing contract;
// `Message` names the operator the admin needs to install. ensure*
// callers reuse this so the message is uniform across data tiers.
func dependencyMissingCondition(condType, depOperator string) metav1.Condition {
	return metav1.Condition{
		Type:    condType,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonDependencyOperatorMissing,
		Message: fmt.Sprintf("%s CRDs are not installed in this cluster — install %s and restart the mage-operator pod", depOperator, depOperator),
	}
}
