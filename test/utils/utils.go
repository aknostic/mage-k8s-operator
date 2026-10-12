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

package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

const (
	certmanagerVersion = "v1.19.2"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	defaultKindBinary  = "kind"
	defaultKindCluster = "kind"
)

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}

	// Delete leftover leases in kube-system (not cleaned by default)
	kubeSystemLeases := []string{
		"cert-manager-cainjector-leader-election",
		"cert-manager-controller",
	}
	for _, lease := range kubeSystemLeases {
		cmd = exec.Command("kubectl", "delete", "lease", lease,
			"-n", "kube-system", "--ignore-not-found", "--force", "--grace-period=0")
		if _, err := Run(cmd); err != nil {
			warnError(err)
		}
	}
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err := Run(cmd)
	return err
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cluster := defaultKindCluster
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}
	cmd := exec.Command(kindBinary, kindOptions...)
	_, err := Run(cmd)
	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.SplitSeq(output, "\n")
	for element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// ApplyManifest applies a YAML manifest to the cluster via stdin.
// The MageStore CR can grow large enough that templating it inline in
// Go is awkward; callers build the YAML as a string (heredoc / fmt)
// and pass it through. Idempotent — `kubectl apply` patches an
// existing resource rather than erroring.
func ApplyManifest(yaml string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	_, err := Run(cmd)
	return err
}

// DeleteResource deletes a named resource. Used to remove failed Jobs
// during the upgrade-failure unstick path.
func DeleteResource(kind, name, namespace string) error {
	cmd := exec.Command("kubectl", "delete", kind, name,
		"-n", namespace, "--ignore-not-found")
	_, err := Run(cmd)
	return err
}

// PatchMageStore applies a strategic-merge patch to the named
// MageStore. Used for `spec.image.tag` flips during the upgrade-arc
// tests. JSON-merge semantics on top-level keys mirror what an admin
// would do via `kubectl patch`.
func PatchMageStore(name, namespace, patchJSON string) error {
	cmd := exec.Command("kubectl", "patch", "magestore", name,
		"-n", namespace, "--type=merge", "-p", patchJSON)
	_, err := Run(cmd)
	return err
}

// GetMageStoreCondition reads a single condition off a MageStore via
// `-o jsonpath`. Returns the empty struct when the condition is not
// yet present (a freshly-applied store may not have the upgrade
// conditions surfaced yet); callers Eventually-poll until the
// condition appears.
type Condition struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

func GetMageStoreCondition(name, namespace, condType string) (Condition, error) {
	jp := fmt.Sprintf(
		`-o=jsonpath={range .status.conditions[?(@.type=="%s")]}{.status}|{.reason}|{.message}{end}`,
		condType)
	cmd := exec.Command("kubectl", "get", "magestore", name, "-n", namespace, jp)
	out, err := Run(cmd)
	if err != nil {
		return Condition{}, err
	}
	parts := strings.SplitN(strings.TrimSpace(out), "|", 3)
	if len(parts) < 1 || parts[0] == "" {
		return Condition{Type: condType}, nil
	}
	c := Condition{Type: condType, Status: parts[0]}
	if len(parts) >= 2 {
		c.Reason = parts[1]
	}
	if len(parts) >= 3 {
		c.Message = parts[2]
	}
	return c, nil
}

// GetMageStoreFieldJSONPath reads an arbitrary jsonpath off a MageStore.
// Returns the trimmed string output. Useful for status fields like
// `status.observedImageTag` that don't fit the Condition shape.
func GetMageStoreFieldJSONPath(name, namespace, jsonpath string) (string, error) {
	cmd := exec.Command("kubectl", "get", "magestore", name, "-n", namespace,
		"-o", fmt.Sprintf("jsonpath=%s", jsonpath))
	out, err := Run(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ExecInPod runs `kubectl exec` against a named pod with the given
// command + args. Returns combined output. Used to drive
// `bin/magento setup:perf:generate-fixtures` against the live store
// during the upgrade-arc test.
func ExecInPod(namespace, pod string, command ...string) (string, error) {
	args := append([]string{"exec", "-n", namespace, pod, "--"}, command...)
	cmd := exec.Command("kubectl", args...)
	return Run(cmd)
}

// RetagImage retags a local Docker image and reloads it into the kind
// cluster. The kind-load step is required because `docker tag` only
// affects the local Docker daemon — the kind cluster's containerd
// only sees images loaded explicitly via `kind load docker-image`.
//
// Cluster name comes from the KIND_CLUSTER env var (the same one the
// Makefile sets). Defaults to `mage-operator-test-e2e` to match
// `make test-e2e`.
func RetagImage(srcRef, dstRef string) error {
	cmd := exec.Command("docker", "tag", srcRef, dstRef)
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("docker tag %s %s: %w", srcRef, dstRef, err)
	}
	cluster := "mage-operator-test-e2e"
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindBin := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBin = v
	}
	cmd = exec.Command(kindBin, "load", "docker-image", dstRef, "--name", cluster)
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("kind load %s: %w", dstRef, err)
	}
	return nil
}

// CurlResponse is the shape returned by both CurlURL and
// CurlViaPortForward. Bare-int status + body lets tests use Gomega
// matchers like Equal(503) and ContainSubstring("we'll be right back")
// without dragging net/http into the assertion path.
type CurlResponse struct {
	StatusCode int
	Body       string
}

// CurlURL fetches a URL and returns status + body. Uses the local
// curl binary rather than net/http because the tests already shell
// out for everything else and curl handles redirects, timeouts,
// retries, and TLS-skip with one consistent flag set. `-L` follows
// redirects: Magento configured for localhost:8080 will 302 a
// request whose Host header is `127.0.0.1:<random-port>` (the
// shape `kubectl port-forward` produces) so the final-status
// assertion needs the redirect chain followed to reach the
// storefront response.
func CurlURL(url string) (CurlResponse, error) {
	// -s silent, -o /dev/null body, -w %{http_code} status; second
	// curl pulls the body. Two invocations is cheaper than parsing
	// curl -i output.
	statusCmd := exec.Command("curl", "-sS", "-L", "-k", "-o", "/dev/null",
		"-w", "%{http_code}", "--max-time", "30", url)
	statusOut, err := statusCmd.Output()
	if err != nil {
		return CurlResponse{}, fmt.Errorf("curl status %s: %w", url, err)
	}
	status, err := strconv.Atoi(strings.TrimSpace(string(statusOut)))
	if err != nil {
		return CurlResponse{}, fmt.Errorf("parse status %q: %w", string(statusOut), err)
	}
	bodyCmd := exec.Command("curl", "-sS", "-L", "-k", "--max-time", "30", url)
	bodyOut, err := bodyCmd.Output()
	if err != nil {
		// Body fetch failed but we got a status — return what we have
		// so the test can still assert on status alone.
		return CurlResponse{StatusCode: status}, nil
	}
	return CurlResponse{StatusCode: status, Body: string(bodyOut)}, nil
}

// PortForward starts `kubectl port-forward` in the background against
// the named target (e.g. `deployment/store-frontend`) and returns the
// chosen local port plus a cleanup function. The cleanup func sends
// SIGTERM to the process group so the port-forward and any kubectl
// children exit together.
//
// The verification-window contract in ADR-008 hinges on this: port-
// forward operates at the kubelet level and bypasses Service-selector
// flips, so it can reach workload pods even while the public Ingress
// serves the maintenance 503.
func PortForward(namespace, target string, remotePort int) (int, func(), error) {
	localPort, err := pickFreePort()
	if err != nil {
		return 0, nil, err
	}
	cmd := exec.Command("kubectl", "port-forward",
		"-n", namespace, target,
		fmt.Sprintf("%d:%d", localPort, remotePort))
	// Put the child in its own process group so the cleanup SIGTERM
	// reaches kubectl-port-forward AND its forked subprocesses.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("start port-forward: %w", err)
	}
	cleanup := func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			_, _ = cmd.Process.Wait()
		}
	}
	// Wait for the port to start accepting connections. kubectl
	// port-forward prints "Forwarding from 127.0.0.1:NNNN" once
	// it's ready, but polling the port is simpler and doesn't
	// require capturing stderr.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp",
			fmt.Sprintf("127.0.0.1:%d", localPort), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return localPort, cleanup, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	cleanup()
	return 0, nil, fmt.Errorf("port-forward to %s never became ready", target)
}

// pickFreePort asks the kernel for an unused TCP port by binding to
// :0 and reading the chosen port off the listener. Closing the
// listener leaves the port in TIME_WAIT briefly, but kubectl
// port-forward sets SO_REUSEADDR so the rebind is clean.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}
