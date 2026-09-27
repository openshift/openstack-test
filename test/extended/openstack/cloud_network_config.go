// test/extended/openstack/cloud_network_config.go
package openstack

import (
	"context"
	"fmt"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	exutil "github.com/openshift/origin/test/extended/util"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

const (
	cloudNetworkConfigCMName      = "cloud-network-config"
	cloudNetworkConfigCMNamespace = "openshift-network-operator"
	maxAllowedAddressPairsKey     = "platform-os-max-allowed-address-pairs"
	cnccDeploymentName            = "cloud-network-config-controller"
	cnccNamespace                 = "openshift-cloud-network-config-controller"
	cnccContainerName             = "controller"
)

var _ = g.Describe("[OTP][sig-installer][Suite:openshift/openstack][cloud-network-config] The cloud-network-config ConfigMap", func() {
	oc := exutil.NewCLI("openstack")
	var clientSet *kubernetes.Clientset

	g.BeforeEach(func(ctx g.SpecContext) {
		var err error
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.AfterEach(func(ctx g.SpecContext) {
		g.By("Cleaning up: deleting cloud-network-config ConfigMap if present")
		_ = deleteCloudNetworkConfigCM(ctx, clientSet)

		g.By("Cleaning up: waiting for network operator to recover")
		waitForNetworkOperatorRecovered(ctx, oc)

		g.By("Cleaning up: waiting for CNCC to be available")
		o.Eventually(func(g o.Gomega) {
			dep, err := clientSet.AppsV1().Deployments(cnccNamespace).Get(ctx, cnccDeploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(o.HaveOccurred())
			g.Expect(dep.Status.AvailableReplicas).To(o.BeNumerically(">=", 1))
		}, "5m", "5s").WithContext(ctx).Should(o.Succeed())
	})

	g.It("should configure CNCC with custom max_allowed_address_pairs and update on change", func(ctx g.SpecContext) {
		// --- Baseline ---
		g.By("Recording baseline CNCC deployment generation")
		dep, err := getCNCCDeployment(ctx, clientSet)
		o.Expect(err).NotTo(o.HaveOccurred())
		baseGeneration := dep.Generation

		g.By("Asserting the max_allowed_address_pairs flag is absent at baseline")
		args, err := getCNCCContainerArgs(ctx, clientSet)
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, arg := range args {
			o.Expect(arg).NotTo(o.ContainSubstring(maxAllowedAddressPairsKey),
				"expected flag to be absent at baseline but found: %q", arg)
		}

		g.By("Recording baseline node egress IP capacity")
		workers, err := clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/worker,!node-role.kubernetes.io/infra",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(workers.Items).NotTo(o.BeEmpty(), "no worker nodes found")
		workerName := workers.Items[0].Name

		baselineCapacity, err := getEgressIPCapacityFromNode(workers.Items[0])
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("Baseline egress IP capacity on node %s: %d", workerName, baselineCapacity)

		// --- Create ConfigMap ---
		g.By("Creating cloud-network-config ConfigMap with platform-os-max-allowed-address-pairs=10")
		o.Expect(setCloudNetworkConfigCM(ctx, clientSet, "10")).To(o.Succeed())

		g.By("Waiting for CNCC to roll out with the new configuration")
		waitForCNCCRolloutAfter(ctx, clientSet, baseGeneration)

		g.By("Asserting CNCC Deployment args contain the flag with value 10")
		args, err = getCNCCContainerArgs(ctx, clientSet)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(args).To(o.ContainElement("-platform-os-max-allowed-address-pairs=10"),
			"expected -platform-os-max-allowed-address-pairs=10 in args: %v", args)

		g.By("Asserting node egress IP capacity is unchanged (delta=0: configured value equals default)")
		o.Eventually(func(g o.Gomega) {
			node, err := clientSet.CoreV1().Nodes().Get(ctx, workerName, metav1.GetOptions{})
			g.Expect(err).NotTo(o.HaveOccurred())
			capacity, err := getEgressIPCapacityFromNode(*node)
			g.Expect(err).NotTo(o.HaveOccurred())
			g.Expect(capacity).To(o.Equal(baselineCapacity),
				"capacity should be unchanged since configured value equals the default")
		}, "5m", "5s").WithContext(ctx).Should(o.Succeed())

		// --- Delete ConfigMap ---
		g.By("Recording CNCC deployment generation before ConfigMap deletion")
		dep, err = getCNCCDeployment(ctx, clientSet)
		o.Expect(err).NotTo(o.HaveOccurred())
		preDeleteGeneration := dep.Generation

		g.By("Deleting the cloud-network-config ConfigMap")
		o.Expect(deleteCloudNetworkConfigCM(ctx, clientSet)).To(o.Succeed())

		g.By("Waiting for CNCC to roll out after ConfigMap deletion (flag removed from Deployment spec)")
		waitForCNCCRolloutAfter(ctx, clientSet, preDeleteGeneration)

		g.By("Asserting CNCC Deployment args do not contain the max_allowed_address_pairs flag")
		args, err = getCNCCContainerArgs(ctx, clientSet)
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, arg := range args {
			o.Expect(arg).NotTo(o.ContainSubstring(maxAllowedAddressPairsKey),
				"expected flag to be absent after deletion but found: %q", arg)
		}

		g.By("Asserting node egress IP capacity is restored to baseline")
		o.Eventually(func(g o.Gomega) {
			node, err := clientSet.CoreV1().Nodes().Get(ctx, workerName, metav1.GetOptions{})
			g.Expect(err).NotTo(o.HaveOccurred())
			capacity, err := getEgressIPCapacityFromNode(*node)
			g.Expect(err).NotTo(o.HaveOccurred())
			g.Expect(capacity).To(o.Equal(baselineCapacity))
		}, "5m", "5s").WithContext(ctx).Should(o.Succeed())
	})

	g.It("should degrade the network operator for invalid ConfigMap values", func(ctx g.SpecContext) {
		for _, invalidValue := range []string{"0", "-5", "abc"} {
			g.By(fmt.Sprintf("Creating cloud-network-config ConfigMap with invalid value %q", invalidValue))
			o.Expect(setCloudNetworkConfigCM(ctx, clientSet, invalidValue)).To(o.Succeed())

			g.By(fmt.Sprintf("Waiting for network operator to become Degraded with value %q", invalidValue))
			var degradedMessage string
			o.Eventually(func(g o.Gomega) {
				degraded, msg, err := isNetworkOperatorDegraded(ctx, oc)
				g.Expect(err).NotTo(o.HaveOccurred())
				g.Expect(degraded).To(o.BeTrue(), "network operator not yet Degraded for value %q", invalidValue)
				degradedMessage = msg
			}, "3m", "5s").WithContext(ctx).Should(o.Succeed())
			e2e.Logf("Network operator Degraded message for value %q: %s", invalidValue, degradedMessage)
			o.Expect(degradedMessage).To(o.ContainSubstring(maxAllowedAddressPairsKey),
				"degraded message should reference the config key for value %q", invalidValue)

			g.By(fmt.Sprintf("Deleting ConfigMap and waiting for operator to recover after value %q", invalidValue))
			o.Expect(deleteCloudNetworkConfigCM(ctx, clientSet)).To(o.Succeed())
			waitForNetworkOperatorRecovered(ctx, oc)
		}
	})
})

// getCNCCDeployment fetches the CNCC Deployment.
func getCNCCDeployment(ctx context.Context, clientSet *kubernetes.Clientset) (*appsv1.Deployment, error) {
	return clientSet.AppsV1().Deployments(cnccNamespace).Get(ctx, cnccDeploymentName, metav1.GetOptions{})
}

// getCNCCContainerArgs returns the args slice from the CNCC container spec.
func getCNCCContainerArgs(ctx context.Context, clientSet *kubernetes.Clientset) ([]string, error) {
	dep, err := getCNCCDeployment(ctx, clientSet)
	if err != nil {
		return nil, err
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == cnccContainerName {
			return c.Args, nil
		}
	}
	return nil, fmt.Errorf("container %q not found in CNCC deployment", cnccContainerName)
}

// waitForCNCCRolloutAfter polls until the CNCC Deployment has rolled out a new generation
// beyond prevGeneration and at least one replica is available.
func waitForCNCCRolloutAfter(ctx context.Context, clientSet *kubernetes.Clientset, prevGeneration int64) {
	o.Eventually(func(g o.Gomega) {
		dep, err := clientSet.AppsV1().Deployments(cnccNamespace).Get(ctx, cnccDeploymentName, metav1.GetOptions{})
		g.Expect(err).NotTo(o.HaveOccurred())
		g.Expect(dep.Generation).To(o.BeNumerically(">", prevGeneration), "deployment generation has not incremented yet")
		g.Expect(dep.Status.ObservedGeneration).To(o.Equal(dep.Generation), "deployment rollout not yet observed by controller")
		g.Expect(dep.Status.AvailableReplicas).To(o.BeNumerically(">=", 1), "no available replicas yet")
	}, "5m", "5s").WithContext(ctx).Should(o.Succeed())
}

// getEgressIPCapacityFromNode parses the egress-ipconfig annotation and returns capacity.ip.
func getEgressIPCapacityFromNode(node corev1.Node) (int, error) {
	configs, err := parseEgressIPAnnotation(node)
	if err != nil {
		return 0, err
	}
	if configs == nil {
		return 0, fmt.Errorf("annotation %s value is null on node %q", egressIPConfigAnnotationKey, node.Name)
	}
	if len(configs) == 0 {
		return 0, fmt.Errorf("empty %s annotation on node %q", egressIPConfigAnnotationKey, node.Name)
	}
	return configs[0].Capacity.IP, nil
}

// setCloudNetworkConfigCM creates or updates the cloud-network-config ConfigMap with the given value.
func setCloudNetworkConfigCM(ctx context.Context, clientSet *kubernetes.Clientset, value string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cloudNetworkConfigCMName,
			Namespace: cloudNetworkConfigCMNamespace,
		},
		Data: map[string]string{maxAllowedAddressPairsKey: value},
	}
	_, err := clientSet.CoreV1().ConfigMaps(cloudNetworkConfigCMNamespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, err := clientSet.CoreV1().ConfigMaps(cloudNetworkConfigCMNamespace).Get(ctx, cloudNetworkConfigCMName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		existing.Data = map[string]string{maxAllowedAddressPairsKey: value}
		_, err = clientSet.CoreV1().ConfigMaps(cloudNetworkConfigCMNamespace).Update(ctx, existing, metav1.UpdateOptions{})
		return err
	}
	return err
}

// deleteCloudNetworkConfigCM deletes the cloud-network-config ConfigMap, ignoring not-found.
func deleteCloudNetworkConfigCM(ctx context.Context, clientSet *kubernetes.Clientset) error {
	err := clientSet.CoreV1().ConfigMaps(cloudNetworkConfigCMNamespace).Delete(ctx, cloudNetworkConfigCMName, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// isNetworkOperatorDegraded returns whether the network ClusterOperator has Degraded=True,
// along with the condition message.
func isNetworkOperatorDegraded(ctx context.Context, oc *exutil.CLI) (bool, string, error) {
	co, err := oc.AdminConfigClient().ConfigV1().ClusterOperators().Get(ctx, "network", metav1.GetOptions{})
	if err != nil {
		return false, "", err
	}
	for _, cond := range co.Status.Conditions {
		if cond.Type == configv1.OperatorDegraded && cond.Status == configv1.ConditionTrue {
			return true, cond.Message, nil
		}
	}
	return false, "", nil
}

// waitForNetworkOperatorRecovered polls until the network ClusterOperator is not Degraded and is Available.
func waitForNetworkOperatorRecovered(ctx context.Context, oc *exutil.CLI) {
	o.Eventually(func(g o.Gomega) {
		co, err := oc.AdminConfigClient().ConfigV1().ClusterOperators().Get(ctx, "network", metav1.GetOptions{})
		g.Expect(err).NotTo(o.HaveOccurred())
		var degraded, available bool
		for _, cond := range co.Status.Conditions {
			if cond.Type == configv1.OperatorDegraded {
				degraded = cond.Status == configv1.ConditionTrue
			}
			if cond.Type == configv1.OperatorAvailable {
				available = cond.Status == configv1.ConditionTrue
			}
		}
		g.Expect(degraded).To(o.BeFalse(), "network operator still Degraded")
		g.Expect(available).To(o.BeTrue(), "network operator not yet Available")
	}, "3m", "5s").WithContext(ctx).Should(o.Succeed())
}
