# Upgrading the mage-operator Helm chart

## v0.6.0 → v0.6.1: CRD now Helm-managed

`v0.6.1` moves the `MageStore` CRD from the chart's `crds/` directory
into `templates/`. Helm's `crds/` directory is install-only — `helm
upgrade` silently leaves the previous CRD schema in place. Chart users
who upgraded from `v0.5.x` to `v0.6.0` saw `kubectl apply` of a
`v0.6.0` `MageStore` manifest fail with
`unknown field "spec.smtp"` / `unknown field "spec.media.seedFromImage"`
because the `v0.5.x` schema was still active.

From `v0.6.1` onward:

- `helm install` — unchanged. The CRD is created as part of the
  install (Helm orders `CustomResourceDefinition` ahead of
  `Deployment` in its built-in install ordering).
- `helm upgrade` — **applies the latest CRD schema**. New fields land
  immediately after `helm upgrade mage-operator …`.
- `helm uninstall` — leaves the CRD in place. The CRD carries
  `helm.sh/resource-policy: keep`, which Helm honors as "do not
  delete on uninstall." Existing `MageStore` CRs survive uninstall;
  delete them yourself if you want a clean slate
  (`kubectl delete magestore --all -A` followed by
  `kubectl delete crd magestores.mage.aknostic.com`).

### One-time step when upgrading from `v0.6.0` to `v0.6.1`

In `v0.6.0` and earlier, the chart shipped its CRD via Helm's
install-only `crds/` directory. The in-cluster CRD therefore carries
no Helm ownership metadata. `v0.6.1` declares the same CRD inside
`templates/`, so `helm upgrade` will refuse the change with:

```
Error: UPGRADE FAILED: Unable to continue with update: CustomResourceDefinition
"magestores.mage.aknostic.com" in namespace "" cannot be imported into the
current release: invalid ownership metadata; ...
```

Adopt the existing CRD into the Helm release before upgrading:

```sh
RELEASE=mage-operator      # whatever you named the Helm release
NAMESPACE=mage-operator    # the namespace `helm install -n` used

kubectl annotate --overwrite crd magestores.mage.aknostic.com \
    meta.helm.sh/release-name="$RELEASE" \
    meta.helm.sh/release-namespace="$NAMESPACE"
kubectl label --overwrite crd magestores.mage.aknostic.com \
    app.kubernetes.io/managed-by=Helm

helm upgrade "$RELEASE" mage-operator/ \
    --repo <chart repo> --version 0.6.1 \
    -n "$NAMESPACE"
```

The `kubectl annotate`/`label` step is the standard Helm 3 adoption
workflow for resources that pre-existed the release. It changes
nothing about the CRD's behaviour or the `MageStore` CRs that depend
on it; it just lets Helm consider the CRD part of the release. After
this one-time adoption, all subsequent upgrades
(`v0.6.1` → `v0.7.0` → …) work with plain `helm upgrade` and Helm
applies the latest CRD schema automatically.

### Why not a pre-install/pre-upgrade hook?

A Helm hook Job would automate the `kubectl apply` step but adds a
moving part (a Job pod, a ServiceAccount, a ClusterRole binding) for
a one-time migration. The `templates/` + `resource-policy: keep`
combination is the documented Helm idiom for chart-managed CRDs and
is what most operators (cert-manager, kube-prometheus-stack, etc.)
use. We follow that convention.

## v0.6.1 → v0.6.2: sticky `image.tag` + reference-image rebuild

`v0.6.2` is bug-fix-only — closes three gaps from the v0.6.1 Kapsule
re-validation. No CRD-shape changes; the upgrade is normally just:

```sh
helm upgrade --install mage-operator mage-operator/ \
    --repo <chart repo> --version 0.6.2 \
    -n mage-operator-system
```

Two surfaces need attention.

### 1. Releases that pinned `image.tag` via `--set` need `--reset-values`

If any prior `helm install`/`helm upgrade` of this release passed
`--set image.tag=<...>`, Helm pinned that value across subsequent
upgrades — chart defaults move forward but the user's `--set`
override stays. `helm upgrade --version 0.6.2` then runs the v0.6.2
chart with the v0.6.1 (or earlier) operator image.

Spot the issue with:

```sh
kubectl -n mage-operator-system get deploy mage-operator \
    -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
# Expect: registry.gitlab.aknostic.com/aknostic/mage-operator:v0.6.2
```

If the printed image still ends in `:v0.6.1` (or an older tag) after
the upgrade, drop the sticky override:

```sh
helm upgrade mage-operator mage-operator/ \
    --repo <chart repo> --version 0.6.2 \
    --reset-values \
    -n mage-operator-system
```

`--reset-values` discards every prior `--set` / `-f` overlay and
re-renders the chart from `values.yaml` defaults. If the original
install legitimately needed non-tag overrides (e.g. resource limits,
imagePullSecrets), re-supply them on the same command line —
`--reset-values` only affects values held *for this release*; the
flags you pass to this `helm upgrade` invocation still apply.

### 2. Workload Deployments need a rollout-restart for the rebuilt reference image

The `magento-reference:2.4.8-sampledata` reference image was rebuilt
in v0.6.2 to fix the queue consumer fast-exit bug
(`consumers_wait_for_messages: 0 → 1`). The image tag is unchanged —
only the underlying digest moved. Workload Deployments use
`imagePullPolicy: IfNotPresent`, so existing pods keep the v0.6.1-era
image even after `helm upgrade` picks up the new operator.

Force the workload pods onto the new digest with:

```sh
NS=<namespace where your MageStore lives>
STORE=<your MageStore name>
kubectl rollout restart deploy \
    -l app.kubernetes.io/instance="$STORE" -n "$NS"
```

The operator's one-shot Jobs (bootstrap, seed, probe, upgrade, store-
URLs) already use `imagePullPolicy: Always` since v0.6.1, so they
pick up the rebuilt image automatically on the next run. Only the
long-lived workload + consumer Deployments need the restart nudge.

### Sequencing

The two notes apply in order: upgrade the operator first (and confirm
its image is `:v0.6.2`), then rollout-restart the workloads in each
MageStore namespace. Skip step 2 entirely if you have no
v0.6.1-or-older workload pods running yet (e.g. brand-new install).
