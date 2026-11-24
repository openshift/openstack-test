// test/extended/openstack/cloud_network_config.go
package openstack

import (
	"context"
	"encoding/json"
	"fmt"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	exutil "github.com/openshift/origin/test/extended/util"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

const (
	maxAllowedAddressPairsKey = "platform-os-max-allowed-address-pairs"
	cnccDeploymentName        = "cloud-network-config-controller"
	cnccNamespace             = "openshift-cloud-network-config-controller"
	cnccContainerName         = "controller"

	// cnccRolloutTimeout bounds how long we wait for CNO to re-render
	// the CNCC Deployment after an Infrastructure change. CNO's
	// operconfig controller does not watch Infrastructure, so the
	// change is only picked up on its periodic resync.
	pollTimeout  = "5m"
	pollInterval = "5s"
)

var infraGVR = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "infrastructures"}

var _ = g.Describe("[OTP][sig-installer][Suite:openshift/openstack][cloud-network-config][OCPFeatureGate:OpenStackMaxAllowedAddressPairs] The Infrastructure API", func() {
	oc := exutil.NewCLI("openstack")
	var clientSet *kubernetes.Clientset
	var limitChanged bool

	g.BeforeEach(func(ctx g.SpecContext) {
		var err error
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())
		infra, err := oc.AdminDynamicClient().Resource(infraGVR).Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		_, exists, err := unstructured.NestedFieldNoCopy(infra.Object, "spec", "platformSpec", "openstack", "maxAllowedAddressPairs")
		o.Expect(err).NotTo(o.HaveOccurred())
		// maxAllowedAddressPairs must be unset before each test
		o.Expect(exists).To(o.BeFalse(), "maxAllowedAddressPairs must be unset at baseline")
	})

	g.AfterEach(func(ctx g.SpecContext) {
		if limitChanged {
			g.By("Cleaning up: restoring Infrastructure maxAllowedAddressPairs")
			// unset maxAllowedAddressPairs
			o.Expect(patchInfrastructureAddressPairs(ctx, oc.AdminDynamicClient(), nil)).To(o.Succeed())
		}
		limitChanged = false

		g.By("Cleaning up: waiting for network operator to recover")
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
		}, pollTimeout, pollInterval).WithContext(ctx).Should(o.Succeed())

		g.By("Cleaning up: waiting for CNCC to be available")
		o.Eventually(func(g o.Gomega) {
			dep, err := clientSet.AppsV1().Deployments(cnccNamespace).Get(ctx, cnccDeploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(o.HaveOccurred())
			g.Expect(dep.Status.AvailableReplicas).To(o.BeNumerically(">=", 1))
		}, pollTimeout, pollInterval).WithContext(ctx).Should(o.Succeed())
	})

	g.It("should configure CNCC with custom max_allowed_address_pairs and update on change", func(ctx g.SpecContext) {
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

		g.By("Setting Infrastructure maxAllowedAddressPairs to 8")
		limitChanged = true
		o.Expect(patchInfrastructureAddressPairs(ctx, oc.AdminDynamicClient(), 8)).To(o.Succeed())

		g.By("Waiting for CNCC to roll out with the new configuration")
		waitForCNCCRolloutAfter(ctx, clientSet, baseGeneration)

		g.By("Asserting CNCC Deployment args contain the flag with value 8")
		args, err = getCNCCContainerArgs(ctx, clientSet)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(args).To(o.ContainElement("-platform-os-max-allowed-address-pairs=8"),
			"expected -platform-os-max-allowed-address-pairs=8 in args: %v", args)

		newCapacity := baselineCapacity - 2
		g.By(fmt.Sprintf("Asserting node egress IP capacity equals %d", newCapacity))
		o.Eventually(func(g o.Gomega) {
			node, err := clientSet.CoreV1().Nodes().Get(ctx, workerName, metav1.GetOptions{})
			g.Expect(err).NotTo(o.HaveOccurred())
			capacity, err := getEgressIPCapacityFromNode(*node)
			g.Expect(err).NotTo(o.HaveOccurred())
			g.Expect(capacity).To(o.Equal(newCapacity),
				fmt.Sprintf("node egress IP capacity should be %d", newCapacity))
		}, pollTimeout, pollInterval).WithContext(ctx).Should(o.Succeed())

		g.By("Recording CNCC deployment generation before restoring Infrastructure")
		dep, err = getCNCCDeployment(ctx, clientSet)
		o.Expect(err).NotTo(o.HaveOccurred())
		preDeleteGeneration := dep.Generation

		g.By("Restoring Infrastructure maxAllowedAddressPairs to its baseline value")
		o.Expect(patchInfrastructureAddressPairs(ctx, oc.AdminDynamicClient(), nil)).To(o.Succeed())

		g.By("Waiting for CNCC to roll out after Infrastructure restoration (flag removed from Deployment spec)")
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
		}, pollTimeout, pollInterval).WithContext(ctx).Should(o.Succeed())
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

func waitForCNCCRolloutAfter(ctx context.Context, clientSet *kubernetes.Clientset, prevGeneration int64) {
	o.Eventually(func(g o.Gomega) {
		dep, err := clientSet.AppsV1().Deployments(cnccNamespace).Get(ctx, cnccDeploymentName, metav1.GetOptions{})
		g.Expect(err).NotTo(o.HaveOccurred())
		g.Expect(dep.Generation).To(o.BeNumerically(">", prevGeneration), "deployment generation has not incremented yet")
		g.Expect(dep.Status.ObservedGeneration).To(o.Equal(dep.Generation), "deployment rollout not yet observed by controller")
		g.Expect(dep.Status.AvailableReplicas).To(o.BeNumerically(">=", 1), "no available replicas yet")
	}, pollTimeout, pollInterval).WithContext(ctx).Should(o.Succeed())
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

func patchInfrastructureAddressPairs(ctx context.Context, client dynamic.Interface, value any) error {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"platformSpec": map[string]any{
				"openstack": map[string]any{"maxAllowedAddressPairs": value},
			},
		},
	})
	if err != nil {
		return err
	}

	_, err = client.Resource(infraGVR).Patch(ctx, "cluster", types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
