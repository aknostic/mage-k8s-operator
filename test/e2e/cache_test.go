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

// Package e2e — Sentinel-aware cache bootstrap regression test
// (v0.4.0 fix A).
//
// What this test exercises: the operator-owned Sentinel HA cache,
// specifically the bootstrap-script behaviour after a Sentinel-driven
// failover. v0.3.x's bootstrap was ordinal-only — pod 0 always booted
// as master regardless of Sentinel's view, so a node loss + re-schedule
// after Sentinel had promoted (say) pod 1 produced a brief split-brain
// window. v0.4.0's bootstrap queries Sentinel first; pod 0 boots as a
// replica of the Sentinel-tracked master when Sentinel reports a
// non-pod-0 master.
//
// What this test does NOT exercise: full Magento Ready=True. The
// fix is local to the cache pipeline; we wait on CacheReady=True
// only, which materialises in 1-2 minutes versus the 25+ minutes
// Magento bootstrap takes. Fence: this test asserts ONLY the
// cache subsystem's behaviour. Any Magento-level regression is
// caught by the upgrade-arc Describe.

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aknostic/mage-k8s-operator/test/utils"
)

const (
	cacheNamespace      = "mage-cache-e2e"
	cacheStoreName      = "store"
	cacheReadyCond      = "CacheReady"
	cacheSentinelDeploy = "store-valkey-sentinel"
	cacheValkeyPod0     = "store-valkey-0"
	cacheValkeyPod0FQDN = "store-valkey-0.store-valkey"
	cacheSentinelPort   = "26379"
	cacheSentinelMaster = "mymaster"
)

var _ = Describe("Cache failover — Sentinel-aware bootstrap", Ordered, func() {

	BeforeAll(func() {
		By("creating the cache-test namespace")
		cmd := exec.Command("kubectl", "create", "ns", cacheNamespace)
		_, err := utils.Run(cmd)
		if err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
			Fail(fmt.Sprintf("failed to create namespace: %v", err))
		}

		By("installing mage-operator CRDs (idempotent)")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the operator (idempotent)")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy controller")

		By("waiting for the controller to become Available")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "wait",
				"-n", "mage-operator-system",
				"--for=condition=Available",
				"--timeout=2m",
				"deployment/mage-operator-controller-manager")
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
		}).WithTimeout(3 * time.Minute).Should(Succeed())

		By("applying a minimal MageStore with cache.redis.replicas=3")
		// Workloads.Frontend is required by the CRD (replicas
		// minimum 1). The frontend pod's image won't pull in this
		// test but that's irrelevant — we only assert against the
		// CacheReady condition, not Ready=True.
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
    frontend:
      replicas: 1
  cache:
    redis:
      replicas: 3
`, cacheStoreName, cacheNamespace, defaultRefImage, "2.4.8")
		Expect(utils.ApplyManifest(manifest)).To(Succeed())

		By("waiting for CacheReady=True (StatefulSet ready + Sentinel quorum)")
		// Cache materialisation is independent of Magento bootstrap.
		// Sentinel HA on a kind cluster needs ~1 min for the 3-pod
		// StatefulSet rollout + Sentinel quorum convergence; budget
		// 5 min to absorb image-pull jitter on a cold node.
		Eventually(func(g Gomega) {
			cond, err := utils.GetMageStoreCondition(cacheStoreName, cacheNamespace, cacheReadyCond)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cond.Status).To(Equal("True"),
				"CacheReady not yet True (reason=%q msg=%q)", cond.Reason, cond.Message)
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	})

	AfterAll(func() {
		By("deleting the test MageStore")
		_ = utils.DeleteResource("magestore", cacheStoreName, cacheNamespace)
		By("removing the cache-test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", cacheNamespace,
			"--ignore-not-found", "--timeout=2m")
		_, _ = utils.Run(cmd)
	})

	Context("master-kill + pod-0-restart arc (v0.3.0 known-gap regression)", func() {

		var promotedMaster string

		It("Sentinel reports pod 0 as the initial master", func() {
			addr, err := sentinelMasterAddr()
			Expect(err).NotTo(HaveOccurred())
			Expect(addr).To(Equal(cacheValkeyPod0FQDN),
				"Sentinel should track pod 0 as master at first reconcile, got %q", addr)
		})

		It("Sentinel promotes a replica when pod 0 is killed", func() {
			By("deleting pod 0 (the current Sentinel master)")
			cmd := exec.Command("kubectl", "delete", "pod", cacheValkeyPod0,
				"-n", cacheNamespace, "--wait=false")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Sentinel to promote a non-pod-0 replica")
			// Sentinel's `down-after-milliseconds 5000` plus the
			// failover-timeout means the new master is observable
			// within ~10-20 seconds; budget 3 min to absorb scheduler
			// + Sentinel re-convergence. Ginkgo shuffles top-level
			// containers, and right after the upgrade-arc spec the
			// cluster is busy enough that 90s was not always enough
			// (failover-timeout 30s lets Sentinel retry a few times).
			Eventually(func(g Gomega) {
				addr, err := sentinelMasterAddr()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(addr).NotTo(Equal(cacheValkeyPod0FQDN),
					"Sentinel still tracks pod 0 — failover hasn't completed yet")
				g.Expect(addr).NotTo(BeEmpty())
				promotedMaster = addr
			}).WithTimeout(3 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
		})

		It("restarted pod 0 boots as replica (not master) after the Sentinel-driven failover", func() {
			Expect(promotedMaster).NotTo(BeEmpty(),
				"prior step should have captured the Sentinel-promoted master")

			By("waiting for pod 0 to be re-scheduled and Running again")
			// The StatefulSet controller re-schedules pod 0 after
			// the previous step's deletion. We need it back online
			// before we can simulate the second restart.
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", cacheValkeyPod0,
					"-n", cacheNamespace, "-o", "jsonpath={.status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("Running"))
			}).WithTimeout(2 * time.Minute).WithPolling(3 * time.Second).Should(Succeed())

			By("deleting pod 0 again to simulate a node-loss restart after the failover")
			cmd := exec.Command("kubectl", "delete", "pod", cacheValkeyPod0,
				"-n", cacheNamespace, "--wait=false")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pod 0 to be re-scheduled and Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", cacheValkeyPod0,
					"-n", cacheNamespace, "-o", "jsonpath={.status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("Running"))
			}).WithTimeout(2 * time.Minute).WithPolling(3 * time.Second).Should(Succeed())

			By("asserting pod 0 reports role:slave (not role:master) via INFO replication")
			// We can't read launch ARGS from /proc/1/cmdline because
			// Valkey/Redis overwrites argv at runtime via
			// setproctitle() — the running process's cmdline shows
			// `valkey-server *:6379` regardless of what was passed
			// to exec. The runtime view via `INFO replication` is the
			// authoritative role check: post-restart, pod 0 must
			// report `role:slave` (replica of the Sentinel-promoted
			// master), not `role:master`. v0.3.x bug surfaced as
			// pod 0 reverting to `role:master` after restart;
			// v0.4.0's Sentinel-aware bootstrap pins it to
			// `role:slave`.
			//
			// VALKEY_PASSWORD is already in the container's env
			// (sourced from the auth Secret at pod-spec time), so
			// valkey-cli can authenticate without a kubectl-side
			// password fetch.
			Eventually(func(g Gomega) {
				out, err := utils.ExecInPod(cacheNamespace, cacheValkeyPod0,
					"sh", "-c", `valkey-cli -a "$VALKEY_PASSWORD" --no-auth-warning INFO replication`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("role:slave"),
					"pod 0 should report role:slave after the Sentinel-driven failover; INFO replication=%q",
					out)
				g.Expect(out).NotTo(ContainSubstring("role:master"),
					"pod 0 must not be running as master after the Sentinel-driven failover; INFO replication=%q",
					out)
			}).WithTimeout(90 * time.Second).WithPolling(3 * time.Second).Should(Succeed())
		})
	})
})

// sentinelMasterAddr queries any Sentinel pod for the current
// `mymaster` master address. Returns just the host (first line of
// `SENTINEL get-master-addr-by-name` output) — that's the comparison
// key against pod 0's FQDN.
func sentinelMasterAddr() (string, error) {
	// Pick a Sentinel pod via deployment selector; kubectl-exec
	// against `deployment/<name>` defers to the first matching pod.
	out, err := utils.Run(exec.Command("kubectl", "exec",
		"-n", cacheNamespace,
		"deployment/"+cacheSentinelDeploy,
		"--",
		"valkey-cli", "-p", cacheSentinelPort,
		"SENTINEL", "get-master-addr-by-name", cacheSentinelMaster))
	if err != nil {
		return "", fmt.Errorf("sentinel query: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}
