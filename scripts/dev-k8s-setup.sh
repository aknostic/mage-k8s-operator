#!/usr/bin/env bash
# mage-operator — create a local kind cluster for operator development
# and install the upstream operators we compose. Dep-operator install
# logic lives in scripts/install-dep-operators.sh so the e2e test
# harness can reuse the same version pins.
#
# What this script does NOT do:
#   - Build or load the operator image (use `make docker-build && make kind-load`)
#   - Deploy the operator chart (use `make deploy` or helm install)
#   - Install the mage-operator CRDs (use `make install`)

set -euo pipefail

CLUSTER_NAME="mage-operator-dev"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
KIND_CONFIG="${REPO_ROOT}/local/kind-config.yaml"

command -v kind >/dev/null    || { echo "kind not installed"; exit 1; }
command -v kubectl >/dev/null || { echo "kubectl not installed"; exit 1; }
command -v helm >/dev/null    || { echo "helm not installed"; exit 1; }
docker info >/dev/null 2>&1   || { echo "Docker is not running"; exit 1; }

if kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  echo "Cluster '${CLUSTER_NAME}' already exists — reusing."
else
  echo "Creating kind cluster '${CLUSTER_NAME}'..."
  kind create cluster --config "${KIND_CONFIG}"
fi

kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null
kubectl wait --for=condition=Ready nodes --all --timeout=120s

# Delegate the helm/kubectl deps work so the e2e harness can call the
# same script after creating its own kind cluster.
"${SCRIPT_DIR}/install-dep-operators.sh"

echo
echo "Ready. Context: kind-${CLUSTER_NAME}"
kubectl get nodes
