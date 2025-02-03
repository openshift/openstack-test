package openstack

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
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

			g.By("Create an additional OpenStack Network")
			extraNetworkName := fmt.Sprintf("%v-%v", "foonet", RandomSuffix())
			networkCreateOpts := networks.CreateOpts{Name: extraNetworkName}
			extraNetwork, err := networks.Create(ctx, networkClient, networkCreateOpts).Extract()
			o.Expect(err).NotTo(o.HaveOccurred(), "Error creating network")
			g.By(fmt.Sprintf("Network %v was created", extraNetwork.Name))
			defer networks.Delete(ctx, networkClient, extraNetwork.ID)

			g.By("Create a new machineSet with an additional network with NoAllowedAddressPairs set to true")
			// Create a machineset and add the extra network to a new machineset
			err = machinev1.AddToScheme(scheme.Scheme)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to add Machine to scheme")
			err = configv1.AddToScheme(scheme.Scheme)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to add Config to scheme")
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
			machineNetworks := make(map[string]string)

			for _, machine := range newMachines {
				for _, net := range objects(machine.Get("spec.providerSpec.value.networks")) {
					machineNetworks[net.Get("uuid").String()] = net.Get("noAllowedAddressPairs").String()
				}
			}
			g.By("Verify value of ports AllowedAddressPairs for all the Machines")

			for _, net := range machineNetworks {
				portListOpts := ports.ListOpts{
					NetworkID: net,
				}
				allPages, err := ports.List(networkClient, portListOpts).AllPages(ctx)
				o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get ports")
				allPorts, err := ports.ExtractPorts(allPages)
				o.Expect(err).NotTo(o.HaveOccurred(), "Failed to extract ports")

				for _, port := range allPorts {
					if strings.Contains(port.Name, newMachinesetParams.Name) {
						if value, exists := machineNetworks[port.NetworkID]; exists && value == "true" {
							// There should be a port in the additional network with
							// an empty AllowedAddressPairs
							o.Expect(len(port.AllowedAddressPairs)).To(o.Equal(0))
						} else {

							// For other ports AllowedAddressPairs shouldn't be empty
							o.Expect(len(port.AllowedAddressPairs)).To(o.Not(o.Equal(0)))
						}
					}
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
