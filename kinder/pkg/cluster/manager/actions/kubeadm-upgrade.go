/*
Copyright 2019 The Kubernetes Authors.

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

package actions

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/pkg/errors"

	K8sVersion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/kubeadm/kinder/pkg/cluster/status"
	"k8s.io/kubeadm/kinder/pkg/constants"
	"k8s.io/kubeadm/kinder/pkg/cri"
)

// KubeadmUpgrade executes the kubeadm upgrade workflow, including also deployment of new
// kubeadm/kubelet/kubectl binaries; for sake of simplicity, drain/uncordon when upgrading nodes
// is not executed.
//
// The implementation assumes that the kubeadm/kubelet/kubectl binaries and all the necessary images
// for the new kubernetes version are available in the /kinder/upgrade/{version} folder.
func KubeadmUpgrade(c *status.Cluster, upgradeVersion *K8sVersion.Version, wait time.Duration) (err error) {
	if upgradeVersion == nil {
		return errors.New("kubeadm-upgrade actions requires the --upgrade-version parameter to be set")
	}

	preloadUpgradeImages(c, upgradeVersion)

	for _, n := range c.K8sNodes() {

		if err := upgradeKubeadmBinary(n, upgradeVersion); err != nil {
			return err
		}

		if n.Name() == c.BootstrapControlPlane().Name() {
			err = kubeadmUpgradeApply(c, n, upgradeVersion, wait)
		} else {
			err = kubeadmUpgradeNode(c, n, upgradeVersion, wait)
		}
		if err != nil {
			return err
		}

		if err := upgradeKubeletKubectl(c, n, upgradeVersion, wait); err != nil {
			return err
		}
	}

	return nil
}

func preloadUpgradeImages(c *status.Cluster, upgradeVersion *K8sVersion.Version) {
	srcFolder := filepath.Join("/kinder", "upgrade", fmt.Sprintf("v%s", upgradeVersion))

	fmt.Println("K8s nodes", len(c.K8sNodes()))
	// load images cached on the node into CRI engine
	// this should be executed on all nodes before running kubeadm upgrade apply in order to
	// get everything in place when kubeadm creates pre-pull daemonsets (if not, this might be blocking in case of
	// images not available on public registry, like e.g. pre-release images)
	for _, n := range c.K8sNodes() {
		n.Infof("pre-loading images required for the upgrade")
		nodeCRI, err := n.CRI()
		if err != nil {
			fmt.Printf("error detecting CRI: %v", err)
			continue
		}

		actionHelper, err := cri.NewActionHelper(nodeCRI)
		if err != nil {
			fmt.Printf("error creating the action helper: %v", err)
			continue
		}

		if err := actionHelper.PreLoadUpgradeImages(n, srcFolder); err != nil {
			fmt.Printf("error PreLoadUpgradeImages: %v", err)
			continue
		}
	}
}

func upgradeKubeadmBinary(n *status.Node, upgradeVersion *K8sVersion.Version) error {
	n.Infof("upgrade kubeadm binary")

	srcFolder := filepath.Join("/kinder", "upgrade", fmt.Sprintf("v%s", upgradeVersion))
	src := filepath.Join(srcFolder, "kubeadm")
	dest := filepath.Join("/usr", "bin", "kubeadm")

	if err := n.Command(
		"ln", "-sf", src, dest,
	).Silent().Run(); err != nil {
		return err
	}
	return nil
}

func kubeadmUpgradeApply(c *status.Cluster, cp1 *status.Node, upgradeVersion *K8sVersion.Version, wait time.Duration) error {
	if err := cp1.Command(
		"kubeadm", "upgrade", "apply", "-f", fmt.Sprintf("v%s", upgradeVersion),
	).RunWithEcho(); err != nil {
		return err
	}

	if err := waitControlPlaneUpgraded(c, cp1, upgradeVersion, wait); err != nil {
		return err
	}

	return nil
}

func kubeadmUpgradeNode(c *status.Cluster, n *status.Node, upgradeVersion *K8sVersion.Version, wait time.Duration) error {
	// waitKubeletHasRBAC waits for the kubelet to have access to the expected config map
	// please note that this is a temporary workaround for a problem we are observing on upgrades while
	// executing node upgrades immediately after control-plane upgrade.
	if err := waitKubeletHasRBAC(c, n, upgradeVersion, wait); err != nil {
		return err
	}

	// After v1.15 there is a unique action that detects the type of node (control-plane vs worker)
	// and adapts accordingly.
	// before two actions where required
	if n.MustKubeadmVersion().AtLeast(constants.V1_15) {
		// kubeadm upgrade node
		if err := n.Command(
			"kubeadm", "upgrade", "node",
		).RunWithEcho(); err != nil {
			return err
		}
	} else {
		if n.IsControlPlane() {
			// kubeadm upgrade node experimental-control-plane
			if err := n.Command(
				"kubeadm", "upgrade", "node", "experimental-control-plane",
			).RunWithEcho(); err != nil {
				return err
			}
		}

		// kubeadm upgrade node config
		if err := n.Command(
			"kubeadm", "upgrade", "node", "config", "--kubelet-version", fmt.Sprintf("v%s", upgradeVersion),
		).RunWithEcho(); err != nil {
			return err
		}
	}

	if n.IsControlPlane() {
		if err := waitControlPlaneUpgraded(c, n, upgradeVersion, wait); err != nil {
			return err
		}
	}

	return nil
}

func upgradeKubeletKubectl(c *status.Cluster, n *status.Node, upgradeVersion *K8sVersion.Version, wait time.Duration) error {
	n.Infof("upgrade kubelet and kubectl binaries")

	srcFolder := filepath.Join("/kinder", "upgrade", fmt.Sprintf("v%s", upgradeVersion))

	// upgrade kubectl
	src := filepath.Join(srcFolder, "kubectl")
	dest := filepath.Join("/usr", "bin", "kubectl")

	if err := n.Command(
		"ln", "-sf", src, dest,
	).Silent().Run(); err != nil {
		return err
	}

	// upgrade kubelet
	src = filepath.Join(srcFolder, "kubelet")
	dest = filepath.Join("/usr", "bin", "kubelet")

	if err := n.Command(
		"ln", "-sf", src, dest,
	).Silent().Run(); err != nil {
		return err
	}

	if err := n.Command(
		"systemctl", "restart", "kubelet",
	).Silent().Run(); err != nil {
		return err
	}

	//write "/kind/version"
	if err := n.Command(
		"echo", fmt.Sprintf("\"%s\"", fmt.Sprintf("v%s", upgradeVersion)), ">", "/kind/version",
	).Silent().Run(); err != nil {
		return err
	}

	if err := waitKubeletUpgraded(c, n, upgradeVersion, wait); err != nil {
		return err
	}

	return nil
}