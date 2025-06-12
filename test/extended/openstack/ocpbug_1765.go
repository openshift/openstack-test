package openstack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

	configv1 "github.com/openshift/api/config/v1"
	machinev1alpha1 "github.com/openshift/api/machine/v1alpha1"
	machinev1 "github.com/openshift/api/machine/v1beta1"
	framework "github.com/openshift/cluster-api-actuator-pkg/pkg/framework"
	"github.com/openshift/openstack-test/test/extended/openstack/client"
	"github.com/openshift/openstack-test/test/extended/openstack/machines"
	exutil "github.com/openshift/origin/test/extended/util"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"

	"k8s.io/client-go/rest"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] Bugfix", func() {
	defer g.GinkgoRecover()

	var dc dynamic.Interface
	var cfg *rest.Config
	var clientSet *kubernetes.Clientset
	var networkClient *gophercloud.ServiceClient
	//	var computeClient *gophercloud.ServiceClient
	oc := exutil.NewCLI("openstack")

	g.BeforeEach(func(ctx g.SpecContext) {
		var err error

		g.By("preparing openshift dynamic client")
		cfg, err = e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		dc, err = dynamic.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		skipUnlessMachineAPIOperator(ctx, dc, clientSet.CoreV1().Namespaces())

		g.By("preparing the openstack client")
		networkClient, err = client.GetServiceClient(ctx, openstack.NewNetworkV2)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to build the OpenStack client")
	})

	g.Context("ocpbug_1765:", func() {
		// Automation for https://issues.redhat.com/browse/OCPBUGS-1765
		g.It("[Serial] noAllowedAddressPairs on one port should not affect other ports", func(ctx g.SpecContext) {
			g.By("Fetching worker machineSets")
			const subnetCIDR = "192.168.77.0/24"
			var rawBytes []byte
			var newProviderSpec machinev1alpha1.OpenstackProviderSpec
			var rclient runtimeclient.Client

			machineSets, err := getMachineSets(ctx, dc)
			o.Expect(err).NotTo(o.HaveOccurred(), "Error getting the workers machinesets")

			if len(machineSets) == 0 {
				e2eskipper.Skipf("Expects at least one worker machineset. Found none.")
			}

			clusterInfra, err := oc.AdminConfigClient().ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred(), "Error creating an Admin config client")

			if clusterInfra.Status.PlatformStatus.OpenStack.LoadBalancer != nil && clusterInfra.Status.PlatformStatus.OpenStack.LoadBalancer.Type == configv1.LoadBalancerTypeUserManaged {
				e2eskipper.Skipf("Test no applicable when using external LoadBalancer. No allowed address is set when using that feature.")
			}

			rclient, err = runtimeclient.New(cfg, runtimeclient.Options{})
			o.Expect(err).NotTo(o.HaveOccurred(), "Error creating a runtime client")

			workerUUID := getFirstWorkerID(clientSet, ctx)
			workerAddressPairs := getInstancePortsAllowedAddressPairs(ctx, networkClient, workerUUID)

			g.By("Create an additional OpenStack Network")
			extraNetworkName := fmt.Sprintf("%v-%v", "foonet", RandomSuffix())
			networkCreateOpts := networks.CreateOpts{Name: extraNetworkName}
			extraNetwork, err := networks.Create(ctx, networkClient, networkCreateOpts).Extract()
			o.Expect(err).NotTo(o.HaveOccurred(), "Error creating network")
			subnetCreateOpts := subnets.CreateOpts{Name: extraNetworkName, NetworkID: extraNetwork.ID, CIDR: subnetCIDR, IPVersion: gophercloud.IPv4}
			_, err = subnets.Create(ctx, networkClient, subnetCreateOpts).Extract()
			o.Expect(err).NotTo(o.HaveOccurred(), "Error creating subnet")

			g.By(fmt.Sprintf("Network %v was created", extraNetwork.Name))
			defer networks.Delete(ctx, networkClient, extraNetwork.ID)

			g.By("Create a new machineSet with an additional network with NoAllowedAddressPairs set to true")
			// Create a machineset and add the extra network to a new machineset
			err = machinev1.AddToScheme(scheme.Scheme)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to add Machine to scheme")
			err = configv1.AddToScheme(scheme.Scheme)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to add Config to scheme")

			// BuildMachineSetParams builds a MachineSetParams object from the first worker MachineSet retrieved from the cluster.
			newMachinesetParams := framework.BuildMachineSetParams(ctx, rclient, 1)
			rawBytes, err = json.Marshal(newMachinesetParams.ProviderSpec.Value)
			o.Expect(err).NotTo(o.HaveOccurred(), "Error marshaling new MachineSet Provider Spec")
			err = json.Unmarshal(rawBytes, &newProviderSpec)
			o.Expect(err).NotTo(o.HaveOccurred(), "Error unmarshaling new MachineSet Provider Spec")
			newMachinesetParams.Name = fmt.Sprintf("%v-%v", "extra-network", RandomSuffix())

			// Set NoAlloedAddressPairs for the additional network to true
			var extraNetworkParam machinev1alpha1.NetworkParam
			extraNetworkParam.NoAllowedAddressPairs = true
			extraNetworkParam.UUID = extraNetwork.ID
			newProviderSpec.Networks = append(newProviderSpec.Networks, extraNetworkParam)
			newProviderSpecJson, err := json.Marshal(newProviderSpec)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to marshal new Machineset provider spec")
			newMachinesetParams.ProviderSpec.Value.Raw = newProviderSpecJson

			ms, err := framework.CreateMachineSet(rclient, newMachinesetParams)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to create a Machineset")

			defer DeleteMachinesetsDefer(rclient, ms)
			err = GetMachinesetRetry(ctx, rclient, ms, true)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get the new Machineset")
			err = waitUntilNMachinesPrefix(ctx, dc, ms.Name, 1)

			newMachines, err := machines.List(ctx, dc, machines.ByMachineSet(ms.Labels["machine.openshift.io/cluster-api-machineset"]))
			o.Expect(err).NotTo(o.HaveOccurred(), "Error fetching machines for MachineSet %q", ms.Name)

			g.By("Verify the number of networks in the new machine is as expected")
			newMachineName := newMachines[0].Get("metadata.name").Str()
			// Wait until the machine is provisioned to get the Openstack instance id
			waitUntilMachinePhase(ctx, dc, newMachineName, "Provisioned")

			newMachine, err := machines.Get(ctx, dc, newMachineName)
			o.Expect(err).NotTo(o.HaveOccurred())

			instanceUUID := strings.TrimPrefix(newMachine.Get("spec.providerID").String(), "openstack:///")
			instanceAddressPairs := getInstancePortsAllowedAddressPairs(ctx, networkClient, instanceUUID)
			o.Expect(len(instanceAddressPairs)).To(o.Equal(len(workerAddressPairs) + 1))

			g.By("Verify that the new machine has the extra network")
			_, ok := instanceAddressPairs[extraNetwork.ID]
			o.Expect(ok).To(o.BeTrue())

			g.By("Verify value of ports AllowedAddressPairs for all the Machines")
			for net, addressPairs := range instanceAddressPairs {
				if net == extraNetwork.ID {
					o.Expect(addressPairs).To(o.Equal(0))
				} else {
					e2e.Logf("Not extra network")
					o.Expect(instanceAddressPairs[net]).To(o.Equal(addressPairs))
				}
			}

			g.By("Deleting the new machineset")
			framework.DeleteMachineSets(rclient, ms)
			err = GetMachinesetRetry(ctx, rclient, ms, false)
			o.Expect(errors.IsNotFound(err)).To(o.BeTrue(), "Machineset %v was not deleted", ms.Name)

			waitUntilNMachinesPrefix(ctx, dc, ms.Name, 0)
			g.By(fmt.Sprintf("Deleting network %v", extraNetworkName))
			networks.Delete(ctx, networkClient, extraNetwork.ID)
		})
	})
})

// getFirstWorkerID returns the ID of the first worker node
func getFirstWorkerID(clientSet *kubernetes.Clientset, ctx context.Context) string {
	workerNodeList, err := clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/worker",
	})
	o.Expect(err).NotTo(o.HaveOccurred())
	worker := workerNodeList.Items[0]
	return strings.TrimPrefix(worker.Spec.ProviderID, "openstack:///")

}

// getInstancePortsAllowedAddressPairs returns a map where the the key is a network ID and the value is
// the length of the port's allowed_address_pairs field in that network attached to instance with ID uuid
func getInstancePortsAllowedAddressPairs(ctx context.Context, client *gophercloud.ServiceClient, uuid string) map[string]int {
	portList := make(map[string]int)

	portListOpts := ports.ListOpts{
		DeviceID: uuid,
	}
	allPages, err := ports.List(client, portListOpts).AllPages(ctx)
	if err != nil {
		return portList
	}
	allPorts, err := ports.ExtractPorts(allPages)
	if err != nil {
		return portList
	}
	for _, port := range allPorts {
		portList[port.NetworkID] = len(port.AllowedAddressPairs)
	}
	return portList
}

// waitUntilMachinePhase waits until a machine phase is the desired one or a timeout is reached
func waitUntilMachinePhase(ctx context.Context, dc dynamic.Interface, machineName string, machinePhase string) {
	wait.PollUntilContextTimeout(ctx, 15*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		machine, err := machines.Get(ctx, dc, machineName)
		if err != nil {
			return false, err
		}
		return machine.Get("status.phase").Str() == machinePhase, nil
	})
}
