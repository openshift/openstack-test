package openstack

import (
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	"github.com/openshift/openstack-test/test/extended/openstack/client"
	"github.com/openshift/openstack-test/test/extended/openstack/machines"
	"github.com/stretchr/objx"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] Bugfix", func() {
	defer g.GinkgoRecover()

	// returns the list of addresses
	getAddressesFromMachine := func(item objx.Map) []string {
		listAddresses := objects(item.Get("status.addresses"))
		result := make([]string, 0, len(listAddresses))
		for _, item := range listAddresses {
			if strings.Contains(fmt.Sprintf("%v", item["type"]), "IP") {
				result = append(result, fmt.Sprintf("%v", item["address"]))
			}
		}
		return result
	}
	// parse instance.Addresses object and return a slice of found IPs
	parseInstanceAddresses := func(addresses map[string]interface{}) ([]string, error) {
		result := make([]string, 0, len(addresses))
		s := fmt.Sprintf("%v", reflect.ValueOf(addresses))

		for _, entry := range strings.Split(s, " ") {
			if strings.HasPrefix(entry, "addr") {
				addrLine := strings.SplitN(entry, ":", 2)
				if len(addrLine) > 1 {
					addr := net.ParseIP(addrLine[1])
					if addr == nil {
						err := fmt.Errorf("invalid ip format: %v", addrLine[1])
						return nil, err
					}
					result = append(result, addr.String())
				}
			}
		}
		return result, nil
	}

	var dc dynamic.Interface
	var clientSet *kubernetes.Clientset
	var computeClient *gophercloud.ServiceClient

	g.BeforeEach(func(ctx g.SpecContext) {
		g.By("preparing openshift dynamic client")
		cfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		dc, err = dynamic.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		skipUnlessMachineAPIOperator(ctx, dc, clientSet.CoreV1().Namespaces())

		g.By("preparing the openstack client")
		computeClient, err = client.GetServiceClient(ctx, openstack.NewComputeV2)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to build the OpenStack client")
	})

	g.Context("bz_2022627:", func() {
		g.It("Machine should report all openstack instance addresses", func(ctx g.SpecContext) {
			// N.B. A UPI installation will not have any Machine objects. This test correctly trivially succeeds in that case because it
			// iterates over Machine objects which exist.

			g.By("fetching machines")
			machines, err := machines.List(ctx, dc)
			o.Expect(err).NotTo(o.HaveOccurred())

			// Create 2 maps of machine names to addresses: one from the machine specs, and one from OpenStack
			machineIPs := make(map[string][]string)
			openstackIPs := make(map[string][]string)

			// Fetch IP addresses of OpenStack instances corresponding to the machines
			for _, machine := range machines {
				machineName := machine.Get("metadata.name").String()
				machineAddresses := getAddressesFromMachine(machine)
				machineResourceID := machine.Get("metadata.annotations.openstack-resourceId").String()
				if len(machineAddresses) == 0 || len(machineResourceID) == 0 {
					// Only consider machines that have addresses and a VM in OpenStack.
					continue
				}

				g.By(fmt.Sprintf("Gather Openstack attributes for machine %q", machineName))
				instance, err := servers.Get(ctx, computeClient, machineResourceID).Extract()

				if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
					o.Expect(err).NotTo(o.HaveOccurred(), "Error gathering Openstack info for machine %v", machineName)
					instanceAddresses, err := parseInstanceAddresses(instance.Addresses)
					o.Expect(err).NotTo(o.HaveOccurred(), "Error parsing addresses for instance %q", instance.Name)
					openstackIPs[machineName] = instanceAddresses
				} else {
					// A VM for a Machine is missing from OpenStack. It's possible that the test which created the
					// machine deleted it after we've fetched the list.
					continue
				}
				machineIPs[machineName] = machineAddresses
			}

			// Assert that the maps are equal, ignoring ordering within the map or of addresses
			o.Expect(mapKeys(machineIPs)).To(o.ConsistOf(mapKeys(openstackIPs)), "Machine names do not match")
			for machineName, machineIPs := range machineIPs {
				o.Expect(machineIPs).To(o.ConsistOf(openstackIPs[machineName]), "Addresses do not match for machine %q", machineName)
			}
		})
	})
})
