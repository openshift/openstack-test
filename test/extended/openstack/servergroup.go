package openstack

import (
	"context"
	"strings"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/servergroups"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	yaml "gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	clientcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] The OpenStack platform", func() {
	defer g.GinkgoRecover()

	var computeClient *gophercloud.ServiceClient
	var ctx context.Context
	var dc dynamic.Interface
	var oc *exutil.CLI
	var masterInstanceUUIDs []interface{}
	var workerInstanceUUIDs []interface{}
	var masterNodeList *corev1.NodeList
	var workerNodeList *corev1.NodeList
	var ms dynamic.NamespaceableResourceInterface
	var controlPlaneGroupName string
	var workerAZGroupNameMap map[string]string

	oc = exutil.NewCLI("openstack")

	g.BeforeEach(func() {
		ctx = context.Background()

		g.By("preparing a dynamic client")
		cfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		dc, err = dynamic.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("getting the IDs of the Control plane and Worker instances")
		{
			masterInstanceUUIDs = make([]interface{}, 0, 3)
			clientSet, err := e2e.LoadClientset()
			o.Expect(err).NotTo(o.HaveOccurred())

			masterNodeList, err = clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
				LabelSelector: "node-role.kubernetes.io/master",
			})
			o.Expect(err).NotTo(o.HaveOccurred())

			workerNodeList, err = clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
				LabelSelector: "node-role.kubernetes.io/worker",
			})
			o.Expect(err).NotTo(o.HaveOccurred())

			ms = dc.Resource(schema.GroupVersionResource{
				Group:    "machine.openshift.io",
				Resource: "machines",
				Version:  "v1beta1",
			})
			for _, item := range masterNodeList.Items {
				uuid := strings.TrimPrefix(item.Spec.ProviderID, "openstack:///")
				masterInstanceUUIDs = append(masterInstanceUUIDs, uuid)

			}
			for _, item := range workerNodeList.Items {
				uuid := strings.TrimPrefix(item.Spec.ProviderID, "openstack:///")
				workerInstanceUUIDs = append(workerInstanceUUIDs, uuid)

			}
			computeClient, err = client(serviceCompute)
			o.Expect(err).NotTo(o.HaveOccurred())
		}
	})

	// OCP 4.5: https://issues.redhat.com/browse/OSASINFRA-1300
	g.It("creates Control plane nodes in a server group", func() {
		g.By("Checking the Control plane instances are in the same Server Group")
		{
			for _, item := range masterNodeList.Items {
				machineAnnotation := strings.SplitN(item.Annotations["machine.openshift.io/machine"], "/", 2)
				o.Expect(machineAnnotation).To(o.HaveLen(2))

				res, err := ms.Namespace(machineAnnotation[0]).Get(ctx, machineAnnotation[1], metav1.GetOptions{})
				o.Expect(err).NotTo(o.HaveOccurred())

				instancecontrolPlaneGroupNameField := getFromUnstructured(res, "spec", "providerSpec", "value", "serverGroupName")
				o.Expect(instancecontrolPlaneGroupNameField).NotTo(o.BeNil(), "the Server Group name should be present in the Machine definition")

				instancecontrolPlaneGroupName := instancecontrolPlaneGroupNameField.(string)
				o.Expect(instancecontrolPlaneGroupName).NotTo(o.BeEmpty(), "the Server Group name should not be the empty string")
				if controlPlaneGroupName == "" {
					controlPlaneGroupName = instancecontrolPlaneGroupName
				} else {
					o.Expect(instancecontrolPlaneGroupName).To(o.Equal(controlPlaneGroupName), "two Control plane Machines have different controlPlaneGroupName set")
				}
			}
		}
		g.By("Checking the actual members of the Server Group")
		{
			serverGroupsWithThatName, err := serverGroupIDsFromName(computeClient, controlPlaneGroupName)
			o.Expect(serverGroupsWithThatName, err).To(o.HaveLen(1), "the server group name either was not found or is not unique")

			serverGroup, err := servergroups.Get(computeClient, serverGroupsWithThatName[0]).Extract()
			o.Expect(serverGroup.Members, err).To(o.ContainElements(masterInstanceUUIDs...))
		}

	})

	// OCP 4.10: https://issues.redhat.com/browse/OSASINFRA-2507
	g.It("creates Control plane nodes on separate hosts when serverGroupPolicy is anti-affinity", func() {
		installConfig, err := installConfigFromCluster(ctx, oc.AdminKubeClient().CoreV1())
		o.Expect(err).NotTo(o.HaveOccurred())
		serverGroupPolicy := installConfig.ControlPlane.Platform.OpenStack.ServerGroupPolicy
		if serverGroupPolicy != "anti-affinity" {
			e2eskipper.Skipf("This test only applies when serverGroupPolicy is set to anti-affinity")
		}
		host_ids := make(map[string]int)

		for _, server_id := range masterInstanceUUIDs {
			server, err := servers.Get(computeClient, server_id.(string)).Extract()
			o.Expect(err).NotTo(o.HaveOccurred())
			host_ids[server.HostID] += 1
		}
		o.Expect(host_ids).To(o.HaveLen(len(masterInstanceUUIDs)),
			"Master nodes should be on different hosts when anti-affinity policy is used")
	})

	g.It("creates Worker nodes in a server group", func() {
		g.By("Checking the Worker instances in a given AZ are in the same Server Group")
		{
			workerAZGroupNameMap = map[string]string{}

			for _, item := range workerNodeList.Items {
				machineAnnotation := strings.SplitN(item.Annotations["machine.openshift.io/machine"], "/", 2)
				o.Expect(machineAnnotation).To(o.HaveLen(2))

				res, err := ms.Namespace(machineAnnotation[0]).Get(ctx, machineAnnotation[1], metav1.GetOptions{})
				o.Expect(err).NotTo(o.HaveOccurred())

				instanceWorkerGroupNameField := getFromUnstructured(res, "spec", "providerSpec", "value", "serverGroupName")
				o.Expect(instanceWorkerGroupNameField).NotTo(o.BeNil(), "the Server Group name should be present in the Machine definition")

				instanceWorkerGroupName := instanceWorkerGroupNameField.(string)
				o.Expect(instanceWorkerGroupName).NotTo(o.BeEmpty(), "the Server Group name should not be the empty string")

				azLabel := item.Labels["topology.kubernetes.io/zone"]
				o.Expect(azLabel).NotTo(o.BeEmpty())

				if workerGroupName, exists := workerAZGroupNameMap[azLabel]; exists {
					o.Expect(instanceWorkerGroupName).To(o.Equal(workerGroupName), "two Worker Machines have different workerGroupName set")
				} else {
					workerAZGroupNameMap[azLabel] = instanceWorkerGroupName
				}
			}
		}
		g.By("Checking the actual members of the Server Group")
		{
			totalMembers := []string{}
			for _, workerGroupName := range workerAZGroupNameMap {
				serverGroupsWithThatName, err := serverGroupIDsFromName(computeClient, workerGroupName)
				o.Expect(serverGroupsWithThatName, err).To(o.HaveLen(1), "the Server Group name either was not found or is not unique")

				serverGroup, err := servergroups.Get(computeClient, serverGroupsWithThatName[0]).Extract()
				o.Expect(err).NotTo(o.HaveOccurred())
				totalMembers = append(totalMembers, serverGroup.Members...)
			}
			o.Expect(totalMembers).To(o.ContainElements(workerInstanceUUIDs...))
		}

	})

	// OCP 4.10: https://issues.redhat.com/browse/OSASINFRA-2570
	g.It("creates Worker nodes on separate hosts when serverGroupPolicy is anti-affinity", func() {
		installConfig, err := installConfigFromCluster(ctx, oc.AdminKubeClient().CoreV1())
		o.Expect(err).NotTo(o.HaveOccurred())
		serverGroupPolicy := installConfig.Compute[0].Platform.OpenStack.ServerGroupPolicy
		if serverGroupPolicy != "anti-affinity" {
			e2eskipper.Skipf("This test only applies when serverGroupPolicy is set to anti-affinity")
		}
		host_ids := make(map[string]int)

		for _, server_id := range workerInstanceUUIDs {
			server, err := servers.Get(computeClient, server_id.(string)).Extract()
			o.Expect(err).NotTo(o.HaveOccurred())
			host_ids[server.HostID] += 1
		}
		o.Expect(host_ids).To(o.HaveLen(len(workerInstanceUUIDs)),
			"Worker nodes should be on different hosts when anti-affinity policy is used")
	})

})

func getFromUnstructured(unstr *unstructured.Unstructured, keys ...string) interface{} {
	m := unstr.UnstructuredContent()
	for _, key := range keys[:len(keys)-1] {
		m = m[key].(map[string]interface{})
	}
	return m[keys[len(keys)-1]]
}

// IDsFromName returns zero or more IDs corresponding to a name. The returned
// error is only non-nil in case of failure.
func serverGroupIDsFromName(client *gophercloud.ServiceClient, name string) ([]string, error) {
	pages, err := servergroups.List(client, nil).AllPages()
	if err != nil {
		return nil, err
	}

	all, err := servergroups.ExtractServerGroups(pages)
	if err != nil {
		return nil, err
	}

	IDs := make([]string, 0, len(all))
	for _, s := range all {
		if s.Name == name {
			IDs = append(IDs, s.ID)
		}
	}

	return IDs, nil
}

type installConfig struct {
	ControlPlane struct {
		Platform struct {
			OpenStack struct {
				ServerGroupPolicy string `yaml:"serverGroupPolicy"`
			} `yaml:"openstack"`
		} `yaml:"platform"`
	} `yaml:"controlPlane"`
	Compute []struct {
		Platform struct {
			OpenStack struct {
				ServerGroupPolicy string `yaml:"serverGroupPolicy"`
			} `yaml:"openstack"`
		} `yaml:"platform"`
	} `yaml:"compute"`
}

func installConfigFromCluster(ctx context.Context, client clientcorev1.ConfigMapsGetter) (installConfig, error) {
	const installConfigName = "cluster-config-v1"

	cm, err := client.ConfigMaps("kube-system").Get(ctx, installConfigName, metav1.GetOptions{})
	if err != nil {
		return installConfig{}, err
	}

	var ic installConfig
	err = yaml.Unmarshal([]byte(cm.Data["install-config"]), &ic)
	return ic, err
}
