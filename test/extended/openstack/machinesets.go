package openstack

import (
	"encoding/json"
	"fmt"
	"strings"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"

	configv1 "github.com/openshift/api/config/v1"
	machinev1 "github.com/openshift/api/machine/v1beta1"
	framework "github.com/openshift/cluster-api-actuator-pkg/pkg/framework"
	machineproviderv1 "github.com/openshift/machine-api-provider-openstack/pkg/apis/openstackproviderconfig/v1alpha1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"k8s.io/client-go/kubernetes/scheme"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] The OpenStack platform", func() {

	defer g.GinkgoRecover()

	var dc dynamic.Interface
	var clientSet *kubernetes.Clientset

	g.Context("after deletion of a machineset", func() {
		g.BeforeEach(func() {
			g.By("preparing openshift dynamic client")
			cfg, err := e2e.LoadConfig()
			o.Expect(err).NotTo(o.HaveOccurred())
			dc, err = dynamic.NewForConfig(cfg)
			o.Expect(err).NotTo(o.HaveOccurred())
			clientSet, err = e2e.LoadClientset()
			o.Expect(err).NotTo(o.HaveOccurred())
		})

		g.It("should not have leftovers ports", func() {
			// Check the scenario at https://bugzilla.redhat.com/show_bug.cgi?id=2073398

			skipUnlessMachineAPIOperator(dc, clientSet.CoreV1().Namespaces())
			g.By("Fetching worker machineSets")
			var networkClient *gophercloud.ServiceClient
			var rawBytes []byte
			var newProviderSpec machineproviderv1.OpenstackProviderSpec
			var rclient runtimeclient.Client

			networkClient, err := client(serviceNetwork)
			o.Expect(err).NotTo(o.HaveOccurred(), "Error creating an openstack network client")
			machineSets, err := listWorkerMachineSets(dc)
			o.Expect(err).NotTo(o.HaveOccurred(), "Error getting the workers machinesets")

			if len(machineSets) == 0 {
				e2eskipper.Skipf("Expects at least one worker machineset. Found none.")
			}

			cfg, err := e2e.LoadConfig()
			o.Expect(err).NotTo(o.HaveOccurred(), "Error getting cluster config")

			rclient, err = runtimeclient.New(cfg, runtimeclient.Options{})
			o.Expect(err).NotTo(o.HaveOccurred(), "Error creating a runtime client")

			err = machinev1.AddToScheme(scheme.Scheme)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to add Machine to scheme")
			err = configv1.AddToScheme(scheme.Scheme)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to add Config to scheme")

			newMachinesetParams := framework.BuildMachineSetParams(rclient, 1)
			rawBytes, err = json.Marshal(newMachinesetParams.ProviderSpec.Value)
			o.Expect(err).NotTo(o.HaveOccurred(), "Error marshaling new MachineSet Provider Spec")

			err = json.Unmarshal(rawBytes, &newProviderSpec)
			o.Expect(err).NotTo(o.HaveOccurred(), "Error unmarshaling new MachineSet Provider Spec")

			newProviderSpec.ServerGroupName = ""
			newProviderSpec.ServerGroupID = "boo"
			newMachinesetParams.Name = fmt.Sprintf("%v-%v", "bogus", RandomSuffix())
			newProviderSpecJson, err := json.Marshal(newProviderSpec)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to marshal new Machineset provider spec")
			newMachinesetParams.ProviderSpec.Value.Raw = newProviderSpecJson

			g.By("Create a new machineSet with bogus server group ID")
			ms, err := framework.CreateMachineSet(rclient, newMachinesetParams)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to create a Machineset")
			defer DeleteMachinesetsDefer(rclient, ms)

			clusterID := ms.Spec.Template.ObjectMeta.Labels["machine.openshift.io/cluster-api-cluster"]
			err = GetMachinesetRetry(rclient, ms, true)

			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get the new Machineset")

			g.By("Deleting the new machineset")
			framework.DeleteMachineSets(rclient, ms)
			err = GetMachinesetRetry(rclient, ms, false)
			o.Expect(errors.IsNotFound(err)).To(o.BeTrue(), "Machineset %v was not deleted", ms.Name)

			networkName := clusterID + "-openshift"

			networkListOpts := networks.ListOpts{Name: networkName}
			networkAllPages, err := networks.List(networkClient, networkListOpts).AllPages()
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get networks")
			allNetworks, err := networks.ExtractNetworks(networkAllPages)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to extract networks")
			networkID := allNetworks[0].ID
			e2e.Logf("Network ID: %v", networkID)
			portListOpts := ports.ListOpts{
				NetworkID: networkID,
			}

			g.By("Checking if an orphaned port related to the delete machineset exists")
			allPages, err := ports.List(networkClient, portListOpts).AllPages()
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get ports")

			allPorts, err := ports.ExtractPorts(allPages)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to extract ports")

			portID := ""
			for _, port := range allPorts {
				if strings.Contains(port.Name, newMachinesetParams.Name) {
					portID = port.ID
				}
			}
			o.Expect(portID).To(o.Equal(""))
		})
	})
})
