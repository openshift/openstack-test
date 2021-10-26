package openstack

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	"github.com/stretchr/objx"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

const (
	scalingTime             = 12 * time.Minute
	operatorWait            = 2 * time.Minute
	machineAPINamespace     = "openshift-machine-api"
	nodeLabelSelectorWorker = "node-role.kubernetes.io/worker"
	machineLabelRole        = "machine.openshift.io/cluster-api-machine-role"
	machineAPIGroup         = "machine.openshift.io"
	machineSetOwningLabel   = "machine.openshift.io/cluster-api-machineset"
)

var _ = g.Describe("[Feature:openstack][Serial] Machines should", func() {
	defer g.GinkgoRecover()
	g.It("grow and decrease again when scaling machineSets", func() {
		// expect new nodes to come up for machineSet
		verifyNodeScalingFunc := func(c *kubernetes.Clientset, dc dynamic.Interface, expectedScaleOut int, machineSet objx.Map) bool {
			nodes, err := getNodesFromMachineSet(c, dc, machineName(machineSet))
			if err != nil {
				e2e.Logf("Error getting nodes from machineSet: %v", err)
				return false
			}
			e2e.Logf("node count : %v, expectedCount %v", len(nodes), expectedScaleOut)
			notReady := false
			for i := range nodes {
				e2e.Logf("node: %v", nodes[i].Name)
				if !isNodeReady(*nodes[i]) {
					e2e.Logf("Node %q is not ready", nodes[i].Name)
					notReady = true
				}
			}
			return !notReady && len(nodes) == expectedScaleOut
		}

		cfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		c, err := e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())
		dc, err := dynamic.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("checking for the openshift machine api operator")
		skipUnlessMachineAPIOperator(dc, c.CoreV1().Namespaces())

		g.By("fetching worker machineSets")
		machineSets, err := listWorkerMachineSets(dc)
		o.Expect(err).NotTo(o.HaveOccurred())
		if len(machineSets) == 0 {
			e2eskipper.Skipf("Expects at least one worker machineset. Found none!!!")
		}

		g.By("checking initial cluster workers size")
		nodeList, err := c.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
			LabelSelector: nodeLabelSelectorWorker,
		})
		o.Expect(err).NotTo(o.HaveOccurred())

		initialNumberOfWorkers := len(nodeList.Items)
		g.By(fmt.Sprintf("initial cluster workers size is %v", initialNumberOfWorkers))

		for _, machineSet := range machineSets {
			initialReplicasMachineSet := getMachineSetReplicaNumber(machineSet)
			expectedScaleOut := initialReplicasMachineSet + 1

			g.By(fmt.Sprintf("scaling %q from %d to %d replicas", machineName(machineSet), initialReplicasMachineSet, expectedScaleOut))
			err = scaleMachineSet(machineName(machineSet), expectedScaleOut)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("checking scaled down worker node is ready")
			o.Eventually(func() bool {
				return verifyNodeScalingFunc(c, dc, expectedScaleOut, machineSet)
			}, scalingTime, 5*time.Second).Should(o.BeTrue())

			g.By(fmt.Sprintf("scaling %q from %d to %d replicas", machineName(machineSet), expectedScaleOut, initialReplicasMachineSet))
			err = scaleMachineSet(machineName(machineSet), initialReplicasMachineSet)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("checking scaled up worker node is ready")
			o.Eventually(func() bool {
				return verifyNodeScalingFunc(c, dc, initialReplicasMachineSet, machineSet)
			}, scalingTime, 5*time.Second).Should(o.BeTrue())
		}

		g.By(fmt.Sprintf("Ensure cluster got back to original size. Final size should be %d worker nodes", initialNumberOfWorkers))
		o.Eventually(func() bool {
			nodeList, err := c.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
				LabelSelector: nodeLabelSelectorWorker,
			})
			o.Expect(err).NotTo(o.HaveOccurred())
			g.By(fmt.Sprintf("got %v nodes, expecting %v", len(nodeList.Items), initialNumberOfWorkers))
			if len(nodeList.Items) != initialNumberOfWorkers {
				return false
			}

			g.By(fmt.Sprintf("ensure openstack instances are in ACTIVE state"))
			computeClient, err := client(serviceCompute)
			o.Expect(err).NotTo(o.HaveOccurred())
			for _, machineSet := range machineSets {
				activeWorkerServers, err := getActiveServerNames(computeClient, machineName(machineSet))
				o.Expect(err).NotTo(o.HaveOccurred())
				if len(activeWorkerServers) != getMachineSetReplicaNumber(machineSet) {
					e2e.Logf("Number of workers matching the machineSet %s on openstack does not match with expected value (%s).",
						machineName(machineSet), getMachineSetReplicaNumber(machineSet))
					return false
				}
			}

			g.By(fmt.Sprintf("ensure workers are healthy"))
			for _, node := range nodeList.Items {
				for _, condition := range node.Status.Conditions {
					switch condition.Type {
					case corev1.NodeReady:
						if condition.Status != corev1.ConditionTrue {
							e2e.Logf("node/%s had unexpected condition %q == %v: %#v", node.Name, condition.Reason, condition.Status)
							return false
						}
					case corev1.NodeMemoryPressure,
						corev1.NodeDiskPressure,
						corev1.NodePIDPressure,
						corev1.NodeNetworkUnavailable:
						if condition.Status != corev1.ConditionFalse {
							e2e.Logf("node/%s had unexpected condition %q == %v: %#v", node.Name, condition.Reason, condition.Status)
							return false
						}

					default:
						e2e.Logf("node/%s had unhandled condition %q == %v: %#v", node.Name, condition.Reason, condition.Status)

					}
				}
				e2e.Logf("node/%s conditions are ok", node.Name)
			}

			return true
		}, operatorWait, 5*time.Second).Should(o.BeTrue())
	})
})

// getActiveServerNames returns a list of names for the Active nodes matching a string. The returned
// error is only non-nil in case of failure.
func getActiveServerNames(client *gophercloud.ServiceClient, substring string) ([]string, error) {
	pages, err := servers.List(client, nil).AllPages()
	if err != nil {
		return nil, err
	}

	all, err := servers.ExtractServers(pages)
	if err != nil {
		return nil, err
	}

	result := make([]string, 0, len(all))
	for _, s := range all {
		if s.Status == "ACTIVE" && strings.Contains(s.Name, substring) {
			result = append(result, s.Name)
		}
	}

	return result, nil
}

// skipUnlessMachineAPI is used to deterine if the Machine API is installed and running in a cluster.
// It is expected to skip the test if it determines that the Machine API is not installed/running.
// Use this early in a test that relies on Machine API functionality.
//
// It checks to see if the machine custom resource is installed in the cluster.
// If machines are not installed it skips the test case.
// It then checks to see if the `openshift-machine-api` namespace is installed.
// If the namespace is not present it skips the test case.
func skipUnlessMachineAPIOperator(dc dynamic.Interface, c coreclient.NamespaceInterface) {
	machineClient := dc.Resource(schema.GroupVersionResource{Group: "machine.openshift.io", Resource: "machines", Version: "v1beta1"})

	err := wait.PollImmediate(time.Second, time.Minute, func() (bool, error) {
		// Listing the resource will return an IsNotFound error when the CRD has not been installed.
		// Otherwise it would return an empty list.
		_, err := machineClient.List(context.Background(), metav1.ListOptions{})
		if err == nil {
			return true, nil
		}
		if errors.IsNotFound(err) {
			e2eskipper.Skipf("The cluster does not support machine instances")
		}
		e2e.Logf("Unable to check for machine api operator: %v", err)
		return false, nil
	})
	o.Expect(err).NotTo(o.HaveOccurred())

	err = wait.PollImmediate(time.Second, time.Minute, func() (bool, error) {
		_, err := c.Get(context.Background(), "openshift-machine-api", metav1.GetOptions{})
		if err == nil {
			return true, nil
		}
		if errors.IsNotFound(err) {
			e2eskipper.Skipf("The cluster machines are not managed by machine api operator")
		}
		e2e.Logf("Unable to check for machine api operator: %v", err)
		return false, nil
	})
	o.Expect(err).NotTo(o.HaveOccurred())
}

func objects(from *objx.Value) []objx.Map {
	var values []objx.Map
	switch {
	case from.IsObjxMapSlice():
		return from.ObjxMapSlice()
	case from.IsInterSlice():
		for _, i := range from.InterSlice() {
			if msi, ok := i.(map[string]interface{}); ok {
				values = append(values, objx.Map(msi))
			}
		}
	}
	return values
}

func nodeNameFromNodeRef(item objx.Map) string {
	return item.Get("status.nodeRef.name").String()
}

// machineClient returns a client for machines scoped to the proper namespace
func machineClient(dc dynamic.Interface) dynamic.ResourceInterface {
	machineClient := dc.Resource(schema.GroupVersionResource{Group: "machine.openshift.io", Resource: "machines", Version: "v1beta1"})
	return machineClient.Namespace(machineAPINamespace)
}

// listMachines list all machines scoped by selector
func listMachines(dc dynamic.Interface, labelSelector string) ([]objx.Map, error) {
	mc := machineClient(dc)
	obj, err := mc.List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}
	machines := objx.Map(obj.UnstructuredContent())
	items := objects(machines.Get("items"))
	return items, nil
}

// machineName returns the machine name
func machineName(item objx.Map) string {
	return item.Get("metadata.name").String()
}

// mapMachineNameToNodeName returns a tuple (map node to machine by name, true if a match is found for every node)
func mapMachineNameToNodeName(machines []objx.Map, nodes []corev1.Node) (map[string]string, bool) {
	result := map[string]string{}
	for i := range machines {
		for j := range nodes {
			if nodes[j].Name == nodeNameFromNodeRef(machines[i]) {
				result[machineName(machines[i])] = nodes[j].Name
				break
			}
		}
	}
	return result, len(machines) == len(result)
}

func isNodeReady(node corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// machineSetClient returns a client for machines scoped to the proper namespace
func machineSetClient(dc dynamic.Interface) dynamic.ResourceInterface {
	machineSetClient := dc.Resource(schema.GroupVersionResource{Group: machineAPIGroup, Resource: "machinesets", Version: "v1beta1"})
	return machineSetClient.Namespace(machineAPINamespace)
}

// listWorkerMachineSets list all worker machineSets
func listWorkerMachineSets(dc dynamic.Interface) ([]objx.Map, error) {
	mc := machineSetClient(dc)
	obj, err := mc.List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	machineSets := []objx.Map{}
	for _, ms := range objects(objx.Map(obj.UnstructuredContent()).Get("items")) {
		e2e.Logf("Labels %v", ms.Get("spec.template.metadata.labels"))
		labels := (*ms.Get("spec.template.metadata.labels")).Data().(map[string]interface{})
		if val, ok := labels[machineLabelRole]; ok {
			if val == "worker" {
				machineSets = append(machineSets, ms)
				continue
			}
		}
	}
	return machineSets, nil
}

func getMachineSetReplicaNumber(item objx.Map) int {
	replicas, _ := strconv.Atoi(item.Get("spec.replicas").String())
	return replicas
}

// getNodesFromMachineSet returns an array of nodes backed by machines owned by a given machineSet
func getNodesFromMachineSet(c *kubernetes.Clientset, dc dynamic.Interface, machineSetName string) ([]*corev1.Node, error) {
	machines, err := listMachines(dc, fmt.Sprintf("%s=%s", machineSetOwningLabel, machineSetName))
	if err != nil {
		return nil, fmt.Errorf("failed to list machines: %v", err)
	}

	// fetch nodes
	allWorkerNodes, err := c.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
		LabelSelector: nodeLabelSelectorWorker,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list worker nodes: %v", err)
	}

	e2e.Logf("Machines found %v, nodes found: %v", machines, allWorkerNodes.Items)
	machineToNodes, match := mapMachineNameToNodeName(machines, allWorkerNodes.Items)
	if !match {
		return nil, fmt.Errorf("not all machines have a node reference: %v", machineToNodes)
	}
	var nodes []*corev1.Node
	for machineName := range machineToNodes {
		node, err := c.CoreV1().Nodes().Get(context.Background(), machineToNodes[machineName], metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get worker nodes %q: %v", machineToNodes[machineName], err)
		}
		nodes = append(nodes, node)
	}

	return nodes, nil
}

func getScaleClient() (scale.ScalesGetter, error) {
	cfg, err := e2e.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("error getting config: %v", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("error discovering client: %v", err)
	}

	groupResources, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		return nil, fmt.Errorf("error getting API resources: %v", err)
	}
	restMapper := restmapper.NewDiscoveryRESTMapper(groupResources)
	scaleKindResolver := scale.NewDiscoveryScaleKindResolver(discoveryClient)

	scaleClient, err := scale.NewForConfig(cfg, restMapper, dynamic.LegacyAPIPathResolverFunc, scaleKindResolver)
	if err != nil {
		return nil, fmt.Errorf("error creating scale client: %v", err)
	}
	return scaleClient, nil
}

// scaleMachineSet scales a machineSet with a given name to the given number of replicas
func scaleMachineSet(name string, replicas int) error {
	scaleClient, err := getScaleClient()
	if err != nil {
		return fmt.Errorf("error calling getScaleClient: %v", err)
	}

	scale, err := scaleClient.Scales(machineAPINamespace).Get(context.Background(), schema.GroupResource{Group: machineAPIGroup, Resource: "MachineSet"}, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error calling scaleClient.Scales get: %v", err)
	}

	scaleUpdate := scale.DeepCopy()
	scaleUpdate.Spec.Replicas = int32(replicas)
	_, err = scaleClient.Scales(machineAPINamespace).Update(context.Background(), schema.GroupResource{Group: machineAPIGroup, Resource: "MachineSet"}, scaleUpdate, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error calling scaleClient.Scales update while setting replicas to %d: %v", err, replicas)
	}
	return nil
}
