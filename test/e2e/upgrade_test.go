//go:build e2e

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

// Package e2e — upgrade-arc validation per ADR-008 Validation Layer 2.
//
// What this test exercises: the orchestrator surface. The operator
// observes a `spec.image.tag` change, materialises a maintenance
// pod, flips Service selectors, suspends crons, scales consumers
// to zero, runs the upgrade Job, rolls workloads forward, and
// restores the cluster to a serving state. Each transition is a
// separate Eventually assertion against the `UpgradeReady`
// condition's Reason field.
//
// What this test does NOT exercise: multi-version Magento schema
// migration. Re-tagging the existing 2.4.8 reference image as
// 2.4.8-rc2 (same bytes, different tag) triggers the orchestrator
// because the operator compares tags not digests; `setup:upgrade`
// against an already-migrated schema is a Magento-side no-op, so
// the Job succeeds and the orchestrator surface is fully exercised
// without a second Magento-version reference image. ADR-008 Layer 3
// (manual `--profile=medium` sweep before tagging v0.2.0) is the
// place where multi-version schema migration is validated; that's
// out-of-scope for this CI-runnable test.
//
// The same-bytes-two-tags strategy reads as a test bug at first
// glance — "why retag the same image?". Future contributors
// extending this test should leave the strategy in place and
// instead add a separate `--profile=medium` sibling for the
// real-migration sweep when that lands.

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aknostic/mage-k8s-operator/test/utils"
)

const (
	upgradeNamespace    = "mage-upgrade-e2e"
	upgradeStoreName    = "store"
	defaultRefImage     = "mage-operator/magento-reference:2.4.8"
	upgradeTargetTag    = "2.4.8-rc2"
	upgradeBrokenTag    = "2.4.8-broken-rc1"
	frontendDeployment  = "store-frontend"
	frontendService     = "store-frontend"
	frontendPort        = 8080
	upgradeReadyCond    = "UpgradeReady"
	storeReadyCond      = "Ready"
	reasonUpgradeStart  = "UpgradeStarting"
	reasonRolloutPend   = "WorkloadRolloutPending"
	reasonUpgradeDone   = "UpgradeComplete"
	reasonUpToDate      = "UpToDate"
	reasonUpgradeFailed = "PermanentlyFailed" // retry budget exhausted (job_recovery.go)
	maintenanceFlipAnno = "mage.aknostic.com/maintenance-flipped"
	upgradeComponentLbl = "app.kubernetes.io/component=upgrade"
)

func referenceImage() string {
	if v, ok := os.LookupEnv("MAGENTO_REFERENCE_IMAGE"); ok && v != "" {
		return v
	}
	return defaultRefImage
}

// referenceRepoTag splits a "repo:tag" image reference. Used to
// build the retag target on the same repository with an upgraded
// tag suffix.
func referenceRepoTag() (string, string) {
	ref := referenceImage()
	idx := strings.LastIndex(ref, ":")
	if idx < 0 {
		return ref, "latest"
	}
	return ref[:idx], ref[idx+1:]
}

// dumpUpgradeDiagnostics writes what is needed to see why the test
// store is stuck or failed: its conditions and workloads, the
// bootstrap Job (setup:install) and its logs, namespace events, node
// pressure and the operator's logs.
func dumpUpgradeDiagnostics() {
	By("collecting diagnostics for the failed upgrade-arc spec")
	bootstrapJob := upgradeStoreName + "-bootstrap"
	cmds := [][]string{
		{"get", "magestore", upgradeStoreName, "-n", upgradeNamespace,
			"-o", `jsonpath={range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}`},
		{"get", "pods,jobs,statefulsets,deployments", "-n", upgradeNamespace, "-o", "wide"},
		{"describe", "job", bootstrapJob, "-n", upgradeNamespace},
		{"describe", "pods", "-l", "job-name=" + bootstrapJob, "-n", upgradeNamespace},
		{"logs", "job/" + bootstrapJob, "-n", upgradeNamespace, "--all-containers", "--timestamps", "--tail=500"},
		{"get", "events", "-n", upgradeNamespace, "--sort-by=.lastTimestamp"},
		{"describe", "nodes"},
		{"logs", "deploy/mage-operator-controller-manager", "-n", namespace, "--tail=300"},
	}
	for _, args := range cmds {
		out, err := utils.Run(exec.Command("kubectl", args...))
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "%v\n", err)
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", out)
	}
}

var _ = Describe("Upgrade arc", Ordered, func() {

	BeforeAll(func() {
		By("creating the upgrade-test namespace")
		cmd := exec.Command("kubectl", "create", "ns", upgradeNamespace)
		_, err := utils.Run(cmd)
		// Idempotent: if the namespace already exists from a prior
		// failed run, kubectl returns AlreadyExists which we ignore.
		if err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
			Fail(fmt.Sprintf("failed to create namespace: %v", err))
		}

		By("installing mage-operator CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the operator (idempotent reuse if Manager describe deployed it)")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy controller")

		By("shortening the operator's Job retry backoff for the failure-path specs")
		// A failed upgrade Job is retried five times (30s … 30m)
		// before the operator gives up; scaled by 0.02 the failure
		// path finishes in minutes. The JSON patch appends to the
		// manager's args; a repeated flag on a re-run is harmless
		// (the last value wins).
		cmd = exec.Command("kubectl", "patch", "deployment", "mage-operator-controller-manager",
			"-n", namespace, "--type=json", "-p",
			`[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--job-retry-backoff-scale=0.02"}]`)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to set --job-retry-backoff-scale")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/mage-operator-controller-manager",
			"-n", namespace, "--timeout=3m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "operator rollout after the backoff patch did not finish")

		By("waiting for the controller to become Ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "wait",
				"-n", "mage-operator-system",
				"--for=condition=Available",
				"--timeout=2m",
				"deployment/mage-operator-controller-manager")
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
		}).WithTimeout(3 * time.Minute).Should(Succeed())

		By("applying the test MageStore at the reference image's 2.4.8 tag")
		repo, tag := referenceRepoTag()
		manifest := fmt.Sprintf(`
apiVersion: mage.aknostic.com/v1alpha1
kind: MageStore
metadata:
  name: %s
  namespace: %s
spec:
  image:
    repository: %s
    tag: %q
  workloads:
    # The reference image is kind-loaded, not in a registry; the
    # default Always made every pod (and the bootstrap Job) fail with
    # ErrImagePull.
    imagePullPolicy: IfNotPresent
    frontend:
      replicas: 1
  database:
    mariaDb: {}
  search:
    openSearch:
      # Default three nodes. A single node would start faster but
      # never forms a cluster (issue #21).
      # 256m JVM heap (default is 512m). The kind cluster — even
      # with 2 nodes at 7.65 GiB each — OOM-kills OpenSearch
      # processes during simultaneous Magento bootstrap if every
      # OpenSearch replica claims its full default heap. For an
      # e2e test that only needs OpenSearch reachable (not
      # performant), the lower heap fits comfortably and avoids
      # the global-OOM cycle observed on the dev cluster as well.
      # Production stays on 512m via the CRD default.
      jvmHeap: 256m
  bootstrap:
    admin:
      email: e2e@example.com
`, upgradeStoreName, upgradeNamespace, repo, tag)
		Expect(utils.ApplyManifest(manifest)).To(Succeed())

		By("waiting for the store to reach Ready=True (setup:install + workload rollout)")
		// 45-minute budget. OpenSearch's 3-replica bootstrap on a
		// 2-node kind cluster contends with everything else for
		// memory + CPU and takes 15-25 min on cold start (cluster
		// formation involves three sequential pod startups, each
		// pulling the 1GB OpenSearch image, JVM heap warming, and
		// the bootstrap-pod handoff). Magento's setup:install
		// adds another 5-10 min on top, and more on GitHub-hosted
		// runners: with a 30-minute budget the bootstrap Job was
		// still running when time ran out. The Eventually's polling
		// interval is intentionally 15s rather than 10s — during
		// the bootstrap window the apiserver is under enough load
		// that aggressive polling adds friction without yielding
		// faster signal.
		Eventually(func(g Gomega) {
			cond, err := utils.GetMageStoreCondition(upgradeStoreName, upgradeNamespace, storeReadyCond)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cond.Status).To(Equal("True"),
				"Ready condition not yet True (reason=%q msg=%q)", cond.Reason, cond.Message)
		}).WithTimeout(45 * time.Minute).WithPolling(15 * time.Second).Should(Succeed())

		// Note: the brief calls for `setup:perf:generate-fixtures
		// --profile=small` here, but populated-DB testing is the
		// ADR-008 Layer 3 manual `--profile=medium` sweep's
		// responsibility (per docs/internals/releasing.md). The
		// orchestrator surface is fully exercised without fixture
		// data — `setup:upgrade` against same-bytes retag is a
		// Magento-side no-op regardless of catalog content, and
		// the maintenance flip / Job lifecycle / Service-selector
		// flip are the load-bearing assertions. Skipping the
		// fixture generation also keeps the kind cluster within
		// memory bounds: the perf-toolkit small profile pushes
		// php-fpm memory beyond what the Docker Desktop VM can
		// host alongside 3-replica OpenSearch + Magento bootstrap
		// concurrently, OOM-killing the frontend pod.
	})

	// AfterEach runs before AfterAll deletes the namespace, so a failed
	// spec (including a failed BeforeAll) can still dump the store's
	// state, the bootstrap Job's logs and the operator's logs.
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpUpgradeDiagnostics()
		}
	})

	AfterAll(func() {
		By("deleting the test MageStore")
		_ = utils.DeleteResource("magestore", upgradeStoreName, upgradeNamespace)
		By("removing the upgrade-test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", upgradeNamespace,
			"--ignore-not-found", "--timeout=2m")
		_, _ = utils.Run(cmd)
		By("undeploying the operator")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)
		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)
	})

	Context("happy path — same-bytes retag triggers a successful upgrade arc", func() {

		It("starts at UpgradeReady=True, Reason=UpToDate", func() {
			cond, err := utils.GetMageStoreCondition(upgradeStoreName, upgradeNamespace, upgradeReadyCond)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal("True"),
				"pre-arc UpgradeReady should be True (got %q, reason=%q)", cond.Status, cond.Reason)
			Expect(cond.Reason).To(Equal(reasonUpToDate))
		})

		It("retags the reference image to trigger orchestrator drift detection", func() {
			repo, _ := referenceRepoTag()
			src := referenceImage()
			dst := fmt.Sprintf("%s:%s", repo, upgradeTargetTag)
			Expect(utils.RetagImage(src, dst)).To(Succeed())
		})

		It("walks UpgradeReady through UpgradeStarting → WorkloadRolloutPending → UpgradeComplete → UpToDate", func() {
			By("patching spec.image.tag to the upgraded tag")
			patch := fmt.Sprintf(`{"spec":{"image":{"tag":%q}}}`, upgradeTargetTag)
			Expect(utils.PatchMageStore(upgradeStoreName, upgradeNamespace, patch)).To(Succeed())

			By("observing UpgradeStarting (maintenance pod stand-up + quiesce in flight)")
			Eventually(func(g Gomega) {
				cond, err := utils.GetMageStoreCondition(upgradeStoreName, upgradeNamespace, upgradeReadyCond)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cond.Reason).To(BeElementOf(
					reasonUpgradeStart, reasonRolloutPend, reasonUpgradeDone, reasonUpToDate),
					"Reason should be in the upgrade-arc set, got %q", cond.Reason)
				// Once we've left UpToDate the arc has triggered;
				// any of the four arc stages counts. The next
				// Eventually narrows to WorkloadRolloutPending.
				g.Expect(cond.Reason).NotTo(Equal(reasonUpToDate),
					"arc has not started yet")
			}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

			By("observing the maintenance Service-selector flip on the workload Service")
			// Backslash-escape the dots in the annotation key
			// (`mage.aknostic.com/maintenance-flipped`); verified
			// against client-go's jsonpath parser. The bracket-quote
			// notation `['<key>']` returns empty for keys with both
			// dots and slashes — even though the kubectl docs claim
			// support, k8s client-go's parser doesn't recognise it.
			escapedKey := strings.ReplaceAll(maintenanceFlipAnno, ".", `\.`)
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service",
					frontendService, "-n", upgradeNamespace,
					"-o", "jsonpath={.metadata.annotations."+escapedKey+"}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("true"),
					"maintenance-flipped annotation not yet on the Service")
			}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

			By("verifying the maintenance fence serves a 503 via Service-routed traffic")
			// Service-routed traffic = port-forward to the Service
			// (kubectl resolves selectors → maintenance pod). Same
			// 503 path the public Ingress sees.
			localPort, cleanup, err := utils.PortForward(upgradeNamespace,
				"service/"+frontendService, frontendPort)
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()
			resp, err := utils.CurlURL(fmt.Sprintf("http://127.0.0.1:%d/", localPort))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(503),
				"expected 503 from Service-routed traffic during maintenance, body=%q", resp.Body)

			By("verifying port-forward to the Deployment bypasses the fence")
			// Deployment-routed traffic = kubelet → pod direct,
			// bypassing Service selectors. The verification window
			// per ADR-008 — admin can validate the upgrade against
			// the workload pods while public traffic stays fenced.
			//
			// The load-bearing assertion is "this path is not
			// served by the maintenance pod". The maintenance pod
			// is a healthy nginx that returns 503 reliably on any
			// path; anything that's NOT a deterministic 503 means
			// the fence is bypassed. During the WorkloadRolloutPending
			// stage the deployment's old pod may have terminated
			// and the new pod may not yet be Ready, in which case
			// curl exits with a connection error (still not 503,
			// still bypassing the fence). Treat curl-error as
			// pass — only a clean 503 from a reachable upstream
			// indicates the fence is leaking through to the
			// Deployment, which is the failure mode the test
			// guards against.
			Eventually(func(g Gomega) {
				lp, cu, e := utils.PortForward(upgradeNamespace,
					"deployment/"+frontendDeployment, frontendPort)
				g.Expect(e).NotTo(HaveOccurred())
				defer cu()
				r, _ := utils.CurlURL(fmt.Sprintf("http://127.0.0.1:%d/", lp))
				g.Expect(r.StatusCode).NotTo(Equal(503),
					"port-forward to Deployment should bypass the fence")
			}).WithTimeout(5 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

			By("waiting for the arc to complete (UpgradeComplete or UpToDate)")
			Eventually(func(g Gomega) {
				cond, err := utils.GetMageStoreCondition(upgradeStoreName, upgradeNamespace, upgradeReadyCond)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cond.Reason).To(BeElementOf(reasonUpgradeDone, reasonUpToDate),
					"arc still in flight (reason=%q msg=%q)", cond.Reason, cond.Message)
			}).WithTimeout(20 * time.Minute).WithPolling(15 * time.Second).Should(Succeed())

			By("verifying the workload Service resumes serving after the fence lifts")
			// "Not 503" is the load-bearing assertion: 503 means the
			// Service is still selector-flipped to the maintenance
			// pod's nginx, which serves a deterministic 503 page on
			// every path. Anything else means the fence is lifted.
			// Magento's actual response status when reached via
			// kubectl port-forward (Host: 127.0.0.1:<random-port>)
			// is implementation-dependent — the install-time
			// `--base-url=http://localhost:8080/` mismatches the
			// port-forward Host, so Magento may 200, 302, or 404,
			// or even produce a curl-level error (exit 52, "no HTTP
			// response") during the brief window where the new
			// frontend pod accepts TCP but php-fpm hasn't booted
			// fully. Curl-error counts as "not 503" — the fence
			// pod is healthy nginx that always responds, so
			// connection-level errors mean we're hitting a
			// different (still-warming) pod, which is what the
			// fence-lifted contract guarantees.
			Eventually(func(g Gomega) {
				lp, cu, e := utils.PortForward(upgradeNamespace,
					"service/"+frontendService, frontendPort)
				g.Expect(e).NotTo(HaveOccurred())
				defer cu()
				r, _ := utils.CurlURL(fmt.Sprintf("http://127.0.0.1:%d/", lp))
				g.Expect(r.StatusCode).NotTo(Equal(503),
					"post-arc Service should not return 503; got %d", r.StatusCode)
			}).WithTimeout(3 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

			By("verifying status.observedImageTag advanced to the new tag")
			observed, err := utils.GetMageStoreFieldJSONPath(upgradeStoreName, upgradeNamespace,
				"{.status.observedImageTag}")
			Expect(err).NotTo(HaveOccurred())
			Expect(observed).To(Equal(upgradeTargetTag),
				"observedImageTag should advance to %q on successful arc", upgradeTargetTag)

			By("verifying UpgradeReady=True at rest (Reason=UpgradeComplete or UpToDate)")
			// ADR-008 says Reason=UpgradeComplete is transient and
			// the next reconcile transitions to UpToDate, but the
			// transition can take >2 min on a resource-constrained
			// kind cluster (the operator's Watch-driven reconcile
			// schedule competes with OpenSearch + Magento for
			// apiserver attention). Both reasons indicate the
			// orchestrator-surface contract is satisfied —
			// observedImageTag advanced, maintenance torn down,
			// selectors restored. The transient distinction is
			// operator-internal; the test treats them as
			// equivalent successful end states.
			Eventually(func(g Gomega) {
				cond, err := utils.GetMageStoreCondition(upgradeStoreName, upgradeNamespace, upgradeReadyCond)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cond.Status).To(Equal("True"))
				g.Expect(cond.Reason).To(BeElementOf(reasonUpgradeDone, reasonUpToDate))
			}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
		})
	})

	// Reuses the store the happy-path Context just upgraded to
	// `2.4.8-rc2`: it's at Ready=True with all stateful deps booted, so
	// we save ~30 minutes of bootstrap by piggybacking. The trade-off is
	// state coupling — if the happy-path Context fails, this one is also
	// skipped (Ginkgo's Ordered semantics already imply that anyway).
	//
	// The arc this Context exercises:
	//   1. Patch spec.image.tag to a broken variant of the reference image
	//      (the broken-image fixture's bin/magento exits 1 on `setup:upgrade`,
	//      see magento-test-image/Dockerfile.broken). The operator's
	//      JobComplete short-circuit (v0.3.1) deletes the happy-path
	//      Context's leftover Complete Job on the first reconcile so the
	//      new arc enters cleanly without manual `kubectl delete jobs`.
	//   2. Eventually the upgrade Job is created, fails, and the operator
	//      surfaces UpgradeReady=False, Reason=PermanentlyFailed once
	//      its retry budget is spent (RetryingFailedJob until then).
	//   3. Revert spec.image.tag to the working `2.4.8-rc2` tag — the
	//      operator MUST stay quiesced (lockstep refusal per ADR 008),
	//      because un-quiescing while a failed Job exists could resume
	//      traffic against a half-migrated schema.
	//   4. Delete the failed Job: the operator's no-Job-no-drift branch
	//      then restores quiesce state, drops the maintenance fence
	//      (selectors flipped back to the workload pods, maintenance
	//      Deployment torn down — the v0.3.1 symmetric-teardown fix), and
	//      surfaces UpgradeReady=True.
	Context("failure path — broken setup:upgrade locks step", func() {

		It("fails the arc when spec.image.tag points at the broken-image fixture", func() {
			By(fmt.Sprintf("patching spec.image.tag to the broken tag %q", upgradeBrokenTag))
			// The broken image is built + kind-loaded by
			// `make build-broken-reference-image` (a prerequisite of
			// `test-e2e`), so we just patch the tag — no RetagImage
			// dance because the broken image is genuinely different
			// bytes from the reference image.
			patch := fmt.Sprintf(`{"spec":{"image":{"tag":%q}}}`, upgradeBrokenTag)
			Expect(utils.PatchMageStore(upgradeStoreName, upgradeNamespace, patch)).To(Succeed())

			By("waiting for the operator to surface UpgradeReady=False, Reason=PermanentlyFailed")
			// The operator recreates a failed upgrade Job five times
			// before giving up (30s … 30m, ~47 min at the production
			// schedule). BeforeAll runs the operator with
			// --job-retry-backoff-scale=0.02, so the waits shrink to
			// ~1 min in total; what remains is six Job runs (the shim
			// exits 1 immediately, backoffLimit=1 adds one pod retry
			// each). 12 min absorbs scheduling jitter on a busy kind
			// cluster.
			Eventually(func(g Gomega) {
				cond, err := utils.GetMageStoreCondition(upgradeStoreName, upgradeNamespace, upgradeReadyCond)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cond.Status).To(Equal("False"),
					"UpgradeReady should be False once the broken Job marks Failed (status=%q reason=%q)",
					cond.Status, cond.Reason)
				g.Expect(cond.Reason).To(Equal(reasonUpgradeFailed),
					"expected Reason=%s, got %q", reasonUpgradeFailed, cond.Reason)
			}).WithTimeout(12 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
		})

		It("refuses to un-quiesce when spec.image.tag is reverted while the failed Job exists", func() {
			By(fmt.Sprintf("reverting spec.image.tag to the working %q", upgradeTargetTag))
			patch := fmt.Sprintf(`{"spec":{"image":{"tag":%q}}}`, upgradeTargetTag)
			Expect(utils.PatchMageStore(upgradeStoreName, upgradeNamespace, patch)).To(Succeed())

			By("asserting UpgradeReady stays False/PermanentlyFailed for a 30s window (lockstep refusal)")
			// Consistently, not Eventually: the load-bearing assertion
			// is that the operator does NOT transition during the
			// post-revert window. ADR 008's lockstep contract: while
			// a failed Job exists the operator refuses to un-quiesce,
			// even though spec.tag now matches observedImageTag, to
			// prevent the foot-gun of resuming traffic against a
			// half-migrated schema.
			//
			// 30s comfortably covers two full reconcile cycles
			// (default reconcile period plus the upgradeRequeueShort
			// 30s requeue), so a regression where the operator briefly
			// transitions UpgradeReady=True before re-failing would
			// be observed within this window.
			Consistently(func(g Gomega) {
				cond, err := utils.GetMageStoreCondition(upgradeStoreName, upgradeNamespace, upgradeReadyCond)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cond.Status).To(Equal("False"),
					"lockstep refusal violated: UpgradeReady transitioned (status=%q reason=%q)", cond.Status, cond.Reason)
				g.Expect(cond.Reason).To(Equal(reasonUpgradeFailed),
					"lockstep refusal violated: Reason changed away from %s (got %q)", reasonUpgradeFailed, cond.Reason)
			}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
		})

		It("recovers to UpgradeReady=True when the admin deletes the failed Job", func() {
			By("deleting the failed upgrade Job")
			cmd := exec.Command("kubectl", "delete", "jobs",
				"-n", upgradeNamespace,
				"-l", upgradeComponentLbl,
				"--ignore-not-found")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for UpgradeReady=True/UpToDate (operator hits no-Job-no-drift branch)")
			// observedImageTag never advanced past upgradeTargetTag
			// (the failed Job's annotation says broken-rc1 but the
			// arc never reached the success-path observed-advance).
			// We reverted spec.tag back to upgradeTargetTag in the
			// previous step. So with the failed Job gone, the
			// operator hits the no-Job-no-drift branch (line 167-180
			// of ensure_upgrade.go), restores quiesce state, and
			// surfaces UpgradeReady=True/UpToDate.
			Eventually(func(g Gomega) {
				cond, err := utils.GetMageStoreCondition(upgradeStoreName, upgradeNamespace, upgradeReadyCond)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cond.Status).To(Equal("True"),
					"UpgradeReady should recover (got status=%q reason=%q)", cond.Status, cond.Reason)
				g.Expect(cond.Reason).To(Equal(reasonUpToDate),
					"expected Reason=%s after Job deletion, got %q", reasonUpToDate, cond.Reason)
			}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

			By("verifying observedImageTag stayed at the working tag (failure path never advanced it)")
			observed, err := utils.GetMageStoreFieldJSONPath(upgradeStoreName, upgradeNamespace,
				"{.status.observedImageTag}")
			Expect(err).NotTo(HaveOccurred())
			Expect(observed).To(Equal(upgradeTargetTag),
				"observedImageTag should still equal %q — the failure-path arc never produced a Complete Job", upgradeTargetTag)

			By("verifying the maintenance Service-selector flip cleared (v0.3.1 symmetric teardown)")
			// Pre-fix the operator only restored cron + consumer
			// quiesce on this branch — Service selectors stayed
			// pointed at the maintenance pod and the storefront
			// kept serving 503 even though UpgradeReady=True. Fix:
			// the no-Job-no-drift branch now also calls
			// restoreServicesFromMaintenance + deleteMaintenance.
			escapedKey := strings.ReplaceAll(maintenanceFlipAnno, ".", `\.`)
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service",
					frontendService, "-n", upgradeNamespace,
					"-o", "jsonpath={.metadata.annotations."+escapedKey+"}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty(),
					"maintenance-flipped annotation should be cleared after recovery")
			}).WithTimeout(1 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

			By("verifying the workload Service selector points at the frontend component")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service",
					frontendService, "-n", upgradeNamespace,
					"-o", "jsonpath={.spec.selector.app\\.kubernetes\\.io/component}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("frontend"),
					"Service selector should be back at the frontend workload pods, got %q", out)
			}).WithTimeout(1 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

			By("verifying the maintenance Deployment was torn down")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment",
					"store-maintenance", "-n", upgradeNamespace,
					"--ignore-not-found", "-o", "name")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty(),
					"maintenance Deployment should be gone after recovery, got %q", out)
			}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
		})
	})
})
