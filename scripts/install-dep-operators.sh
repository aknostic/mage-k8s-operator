#!/usr/bin/env bash
# mage-operator — install the upstream operators we compose into the
# CURRENT kubeconfig context. Pulled out of dev-k8s-setup.sh so the
# e2e test harness (which manages its own kind cluster lifecycle via
# `make setup-test-e2e`) can install the same dep stack without
# duplicating version pins.
#
# Usage:
#   ./scripts/install-dep-operators.sh
#
# Assumes: kubectl, helm, and a working kubeconfig context pointing at
# the target cluster. Does NOT create or tear down clusters.
#
# Version pins match go.mod / docs/prerequisites.md. Bump them in
# lockstep with the matching typed-API versions in go.mod — envtest
# loads CRDs from the module cache, so a helm-chart/go-module skew
# would fail silently.

set -euo pipefail

CERT_MANAGER_VERSION="v1.19.2"
MARIADB_OPERATOR_VERSION="26.3.0"
OPENSEARCH_OPERATOR_VERSION="3.0.2"
RABBITMQ_OPERATOR_VERSION="2.20.1"
GATEWAY_API_VERSION="v1.2.1"
ENVOY_GATEWAY_VERSION="v1.3.0"

command -v kubectl >/dev/null || { echo "kubectl not installed"; exit 1; }
command -v helm    >/dev/null || { echo "helm not installed";    exit 1; }

# cert-manager — opensearch-operator's install manifest references
# cert-manager.io/v1 Certificate and Issuer resources, so the CRDs
# must be present before the opensearch chart installs.
# Uses Helm so we can pass config.enableGatewayAPI=true; without it
# cert-manager's Gateway controller isn't registered and
# cert-manager.io/issuer annotations on Gateway resources are ignored.
# installCRDs=true keeps parity with the previous kubectl apply approach.
echo "Installing cert-manager ${CERT_MANAGER_VERSION}..."
helm repo add jetstack https://charts.jetstack.io >/dev/null 2>&1 || true
helm repo update jetstack >/dev/null
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --version "${CERT_MANAGER_VERSION}" \
  --set installCRDs=true \
  --set config.apiVersion=controller.config.cert-manager.io/v1alpha1 \
  --set config.kind=ControllerConfiguration \
  --set config.enableGatewayAPI=true >/dev/null
kubectl wait --for=condition=Available -n cert-manager \
  deployment/cert-manager-webhook --timeout=5m >/dev/null

# mariadb-operator — two charts, CRDs first (upstream ships them
# separately because the main chart expects the CRDs to pre-exist).
echo "Installing mariadb-operator ${MARIADB_OPERATOR_VERSION}..."
helm repo add mariadb-operator https://helm.mariadb.com/mariadb-operator >/dev/null 2>&1 || true
helm repo update mariadb-operator >/dev/null
helm upgrade --install mariadb-operator-crds mariadb-operator/mariadb-operator-crds \
  --version "${MARIADB_OPERATOR_VERSION}" >/dev/null
helm upgrade --install mariadb-operator mariadb-operator/mariadb-operator \
  --version "${MARIADB_OPERATOR_VERSION}" \
  --namespace mariadb-operator --create-namespace >/dev/null

# opensearch-operator — single chart, CRDs bundled.
echo "Installing opensearch-operator ${OPENSEARCH_OPERATOR_VERSION}..."
helm repo add opensearch-operator https://opensearch-project.github.io/opensearch-k8s-operator >/dev/null 2>&1 || true
helm repo update opensearch-operator >/dev/null
helm upgrade --install opensearch-operator opensearch-operator/opensearch-operator \
  --version "${OPENSEARCH_OPERATOR_VERSION}" \
  --namespace opensearch-operator-system --create-namespace >/dev/null

# Cache (Valkey) is no longer installed via an upstream operator: per
# ADR 009 the mage-operator owns the cache StatefulSet directly to
# avoid Cluster-mode CROSSSLOT failures (Magento issue #39224). No
# operator install needed for `spec.cache.redis` inline mode.

# rabbitmq-cluster-operator — upstream publishes a single-file release
# manifest on GitHub (no Helm chart from the rabbitmq team). Pinned
# to the tag we depend on in go.mod. See ADR 004 for the rationale
# behind picking rabbitmq-cluster-operator for inline RabbitMQ.
echo "Installing rabbitmq-cluster-operator v${RABBITMQ_OPERATOR_VERSION}..."
kubectl apply -f "https://github.com/rabbitmq/cluster-operator/releases/download/v${RABBITMQ_OPERATOR_VERSION}/cluster-operator.yml" >/dev/null

# Gateway API CRDs — installed before Envoy Gateway so the controller
# attaches to existing CRDs cleanly. The ADR 006 amendment (2026-04-24)
# switched the routing layer from ingress-nginx Ingress to Gateway API
# HTTPRoute; the operator emits HTTPRoutes attached to the `eg`
# GatewayClass that Envoy Gateway provisions.
echo "Installing Gateway API CRDs ${GATEWAY_API_VERSION}..."
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" >/dev/null

# Envoy Gateway — installs the Gateway API controller and the
# data-plane provisioner. Each MageStore's Gateway resource triggers
# Envoy Gateway to provision an Envoy Deployment behind it.
echo "Installing Envoy Gateway ${ENVOY_GATEWAY_VERSION}..."
helm upgrade --install eg oci://docker.io/envoyproxy/gateway-helm \
  --version "${ENVOY_GATEWAY_VERSION}" \
  --namespace envoy-gateway-system --create-namespace >/dev/null
kubectl wait --for=condition=Available -n envoy-gateway-system \
  deployment/envoy-gateway --timeout=120s >/dev/null

# `eg` GatewayClass — the operator's HTTPRoutes attach to a
# GatewayClass named `eg` (hard-coded; spec.routing.gatewayClassName
# is a v0.2.0+ punch-list item). The Envoy Gateway helm chart does
# NOT create this GatewayClass for us — we create it explicitly so
# operator-emitted Gateways can find a controller to bind to.
echo "Creating eg GatewayClass..."
kubectl apply -f - <<EOF >/dev/null
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: eg
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
EOF
kubectl wait --for=condition=Accepted gatewayclass/eg --timeout=60s >/dev/null

echo "Dep operators installed."
