/*
Copyright 2021 The Kubernetes Authors.

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

package common

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/version"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	"k8s.io/kubernetes/test/e2e/upgrades"
	"k8s.io/kubernetes/test/utils/junit"
)

// ControlPlaneUpgradeFunc returns a function that performs control plane upgrade.
func ControlPlaneUpgradeFunc(f *framework.Framework, upgCtx *upgrades.UpgradeContext, testCase *junit.TestCase, controlPlaneExtraEnvs []string) func() {
	return func() {
		start := time.Now()
		defer upgrades.FinalizeUpgradeTest(start, testCase)
		target := upgCtx.Versions[1].Version.String()
		framework.ExpectNoError(controlPlaneUpgrade(f, target, controlPlaneExtraEnvs))
		framework.ExpectNoError(checkControlPlaneVersion(f.ClientSet, target))
	}
}

// ClusterUpgradeFunc returns a function that performs full cluster upgrade (both control plane and nodes).
func ClusterUpgradeFunc(f *framework.Framework, upgCtx *upgrades.UpgradeContext, testCase *junit.TestCase, controlPlaneExtraEnvs, nodeExtraEnvs []string) func() {
	return func() {
		start := time.Now()
		defer upgrades.FinalizeUpgradeTest(start, testCase)
		target := upgCtx.Versions[1].Version.String()
		image := upgCtx.Versions[1].NodeImage
		framework.ExpectNoError(controlPlaneUpgrade(f, target, controlPlaneExtraEnvs))
		framework.ExpectNoError(checkControlPlaneVersion(f.ClientSet, target))
		framework.ExpectNoError(nodeUpgrade(f, target, image, nodeExtraEnvs))
		framework.ExpectNoError(checkNodesVersions(f.ClientSet, target))
	}
}

// ClusterDowngradeFunc returns a function that performs full cluster downgrade (both nodes and control plane).
func ClusterDowngradeFunc(f *framework.Framework, upgCtx *upgrades.UpgradeContext, testCase *junit.TestCase, controlPlaneExtraEnvs, nodeExtraEnvs []string) func() {
	return func() {
		start := time.Now()
		defer upgrades.FinalizeUpgradeTest(start, testCase)
		target := upgCtx.Versions[1].Version.String()
		image := upgCtx.Versions[1].NodeImage
		// Yes this really is a downgrade. And nodes must downgrade first.
		framework.ExpectNoError(nodeUpgrade(f, target, image, nodeExtraEnvs))
		framework.ExpectNoError(checkNodesVersions(f.ClientSet, target))
		framework.ExpectNoError(controlPlaneUpgrade(f, target, controlPlaneExtraEnvs))
		framework.ExpectNoError(checkControlPlaneVersion(f.ClientSet, target))
	}
}

const etcdImage = "3.4.9-1"

// controlPlaneUpgrade upgrades control plane node on GCE/GKE.
func controlPlaneUpgrade(f *framework.Framework, v string, extraEnvs []string) error {
	switch framework.TestContext.Provider {
	case "gce":
		return controlPlaneUpgradeGCE(v, extraEnvs)
	case "gke":
		return framework.MasterUpgradeGKE(f.Namespace.Name, v)
	default:
		return fmt.Errorf("controlPlaneUpgrade() is not implemented for provider %s", framework.TestContext.Provider)
	}
}

func controlPlaneUpgradeGCE(rawV string, extraEnvs []string) error {
	env := append(os.Environ(), extraEnvs...)
	// TODO: Remove these variables when they're no longer needed for downgrades.
	if framework.TestContext.EtcdUpgradeVersion != "" && framework.TestContext.EtcdUpgradeStorage != "" {
		env = append(env,
			"TEST_ETCD_VERSION="+framework.TestContext.EtcdUpgradeVersion,
			"STORAGE_BACKEND="+framework.TestContext.EtcdUpgradeStorage,
			"TEST_ETCD_IMAGE="+etcdImage)
	} else {
		// In e2e tests, we skip the confirmation prompt about
		// implicit etcd upgrades to simulate the user entering "y".
		env = append(env, "TEST_ALLOW_IMPLICIT_ETCD_UPGRADE=true")
	}

	v := "v" + rawV
	_, _, err := framework.RunCmdEnv(env, framework.GCEUpgradeScript(), "-M", v)
	return err
}

func traceRouteToControlPlane() {
	traceroute, err := exec.LookPath("traceroute")
	if err != nil {
		framework.Logf("Could not find traceroute program")
		return
	}
	cmd := exec.Command(traceroute, "-I", framework.APIAddress())
	out, err := cmd.Output()
	if len(out) != 0 {
		framework.Logf(string(out))
	}
	if exiterr, ok := err.(*exec.ExitError); err != nil && ok {
		framework.Logf("Error while running traceroute: %s", exiterr.Stderr)
	}
}

// checkControlPlaneVersion validates the control plane version
func checkControlPlaneVersion(c clientset.Interface, want string) error {
	framework.Logf("Checking control plane version")
	var err error
	var v *version.Info
	waitErr := wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		v, err = c.Discovery().ServerVersion()
		if err != nil {
			traceRouteToControlPlane()
			return false, nil
		}
		return true, nil
	})
	if waitErr != nil {
		return fmt.Errorf("CheckControlPlane() couldn't get the control plane version: %v", err)
	}
	// We do prefix trimming and then matching because:
	// want looks like:  0.19.3-815-g50e67d4
	// got  looks like: v0.19.3-815-g50e67d4034e858-dirty
	got := strings.TrimPrefix(v.GitVersion, "v")
	if !strings.HasPrefix(got, want) {
		return fmt.Errorf("control plane had kube-apiserver version %s which does not start with %s", got, want)
	}
	framework.Logf("Control plane is at version %s", want)
	return nil
}

// nodeUpgrade upgrades nodes on GCE/GKE.
func nodeUpgrade(f *framework.Framework, v string, img string, extraEnvs []string) error {
	// Perform the upgrade.
	var err error
	switch framework.TestContext.Provider {
	case "gce":
		err = nodeUpgradeGCE(v, img, extraEnvs)
	case "gke":
		err = nodeUpgradeGKE(f.Namespace.Name, v, img)
	default:
		err = fmt.Errorf("nodeUpgrade() is not implemented for provider %s", framework.TestContext.Provider)
	}
	if err != nil {
		return err
	}
	return waitForNodesReadyAfterUpgrade(f)
}

// TODO(mrhohn): Remove 'enableKubeProxyDaemonSet' when kube-proxy is run as a DaemonSet by default.
func nodeUpgradeGCE(rawV, img string, extraEnvs []string) error {
	v := "v" + rawV
	env := append(os.Environ(), extraEnvs...)
	if img != "" {
		env = append(env, "KUBE_NODE_OS_DISTRIBUTION="+img)
		_, _, err := framework.RunCmdEnv(env, framework.GCEUpgradeScript(), "-N", "-o", v)
		return err
	}
	_, _, err := framework.RunCmdEnv(env, framework.GCEUpgradeScript(), "-N", v)
	return err
}

func nodeUpgradeGKE(namespace string, v string, img string) error {
	framework.Logf("Upgrading nodes to version %q and image %q", v, img)
	nps, err := nodePoolsGKE()
	if err != nil {
		return err
	}
	framework.Logf("Found node pools %v", nps)
	for _, np := range nps {
		args := []string{
			"container",
			"clusters",
			fmt.Sprintf("--project=%s", framework.TestContext.CloudConfig.ProjectID),
			framework.LocationParamGKE(),
			"upgrade",
			framework.TestContext.CloudConfig.Cluster,
			fmt.Sprintf("--node-pool=%s", np),
			fmt.Sprintf("--cluster-version=%s", v),
			"--quiet",
		}
		if len(img) > 0 {
			args = append(args, fmt.Sprintf("--image-type=%s", img))
		}
		_, _, err = framework.RunCmd("gcloud", framework.AppendContainerCommandGroupIfNeeded(args)...)

		if err != nil {
			return err
		}

		framework.WaitForSSHTunnels(namespace)
	}
	return nil
}

func nodePoolsGKE() ([]string, error) {
	args := []string{
		"container",
		"node-pools",
		fmt.Sprintf("--project=%s", framework.TestContext.CloudConfig.ProjectID),
		framework.LocationParamGKE(),
		"list",
		fmt.Sprintf("--cluster=%s", framework.TestContext.CloudConfig.Cluster),
		"--format=get(name)",
	}
	stdout, _, err := framework.RunCmd("gcloud", framework.AppendContainerCommandGroupIfNeeded(args)...)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(stdout)) == 0 {
		return []string{}, nil
	}
	return strings.Fields(stdout), nil
}

func waitForNodesReadyAfterUpgrade(f *framework.Framework) error {
	// Wait for it to complete and validate nodes are healthy.
	//
	// TODO(ihmccreery) We shouldn't have to wait for nodes to be ready in
	// GKE; the operation shouldn't return until they all are.
	numNodes, err := e2enode.TotalRegistered(f.ClientSet)
	if err != nil {
		return fmt.Errorf("couldn't detect number of nodes")
	}
	framework.Logf("Waiting up to %v for all %d nodes to be ready after the upgrade", framework.RestartNodeReadyAgainTimeout, numNodes)
	if _, err := e2enode.CheckReady(f.ClientSet, numNodes, framework.RestartNodeReadyAgainTimeout); err != nil {
		return err
	}
	return nil
}

// checkNodesVersions validates the nodes versions
func checkNodesVersions(cs clientset.Interface, want string) error {
	l, err := e2enode.GetReadySchedulableNodes(cs)
	if err != nil {
		return err
	}
	for _, n := range l.Items {
		// We do prefix trimming and then matching because:
		// want   looks like:  0.19.3-815-g50e67d4
		// kv/kvp look  like: v0.19.3-815-g50e67d4034e858-dirty
		kv, kpv := strings.TrimPrefix(n.Status.NodeInfo.KubeletVersion, "v"),
			strings.TrimPrefix(n.Status.NodeInfo.KubeProxyVersion, "v")
		if !strings.HasPrefix(kv, want) {
			return fmt.Errorf("node %s had kubelet version %s which does not start with %s",
				n.ObjectMeta.Name, kv, want)
		}
		if !strings.HasPrefix(kpv, want) {
			return fmt.Errorf("node %s had kube-proxy version %s which does not start with %s",
				n.ObjectMeta.Name, kpv, want)
		}
	}
	return nil
}
