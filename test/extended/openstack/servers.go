package openstack

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/images"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	"github.com/stretchr/objx"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

const (
	machineAPINamespace   = "openshift-machine-api"
	machineLabelRole      = "machine.openshift.io/cluster-api-machine-role"
	machineAPIGroup       = "machine.openshift.io"
	machineSetOwningLabel = "machine.openshift.io/cluster-api-machineset"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] The OpenStack platform", func() {
	defer g.GinkgoRecover()

	var dc dynamic.Interface
	var clientSet *kubernetes.Clientset
	var computeClient *gophercloud.ServiceClient

	g.Context("on instance creation", func() {

		g.BeforeEach(func() {
			g.By("preparing openshift dynamic client")
			cfg, err := e2e.LoadConfig()
			o.Expect(err).NotTo(o.HaveOccurred())
			dc, err = dynamic.NewForConfig(cfg)
			o.Expect(err).NotTo(o.HaveOccurred())
			clientSet, err = e2e.LoadClientset()
			o.Expect(err).NotTo(o.HaveOccurred())
			g.By("preparing openstack client")
			computeClient, err = client(serviceCompute)
			o.Expect(err).NotTo(o.HaveOccurred())
		})

		g.It("should follow machineset specs", func() {

			skipUnlessMachineAPIOperator(dc, clientSet.CoreV1().Namespaces())
			g.By("fetching worker machineSets")
			machineSets, err := listWorkerMachineSets(dc)
			o.Expect(err).NotTo(o.HaveOccurred())
			if len(machineSets) == 0 {
				e2eskipper.Skipf("Expects at least one worker machineset. Found none.")
			}

			for _, machineSet := range machineSets {
				nameMachineSet := machineSet.Get("metadata.name").String()
				g.By(fmt.Sprintf("Getting the specs from the machineset %q", nameMachineSet))
				replicasMachineSet, _ := strconv.Atoi(machineSet.Get("spec.replicas").String())
				flavorMachineSet := machineSet.Get("spec.template.spec.providerSpec.value.flavor").String()
				imageMachineSet := machineSet.Get("spec.template.spec.providerSpec.value.image").String()
				sgMachineSets := getSecurityGroupNames(machineSet)
				g.By(fmt.Sprintf("Getting the machines specs created by the machineset %q", nameMachineSet))
				machines, err := getMachinesFromMachineSet(dc, nameMachineSet)
				o.Expect(machines, err).To(o.HaveLen(replicasMachineSet),
					"Number of replicas not matching for machineset %q", nameMachineSet)

				for _, machine := range machines {
					machine_name := machine.Get("metadata.name")
					machine_phase := machine.Get("status.phase").String()
					g.By(fmt.Sprintf("Get machine status for machine %q", machine_name))
					o.Expect(machine_phase).To(o.Equal("Running"), "machine status is %q instead of Running", machine_name)
					g.By(fmt.Sprintf("Gather Openstack attributes for machine %q", machine_name))
					instance, err := servers.Get(computeClient, machine.Get("metadata.annotations.openstack-resourceId").String()).Extract()
					o.Expect(err).NotTo(o.HaveOccurred(), "Error gathering Openstack info for machine %v", machine_name)
					g.By(fmt.Sprintf("Compare specs with openstack attributes for machine %q", instance.Name))
					instanceFlavor, err := flavors.Get(computeClient, fmt.Sprintf("%v", instance.Flavor["id"])).Extract()
					o.Expect(err).NotTo(o.HaveOccurred())
					instanceImage, err := images.Get(computeClient, fmt.Sprintf("%v", instance.Image["id"])).Extract()
					o.Expect(err).NotTo(o.HaveOccurred())
					instanceSgs := parseInstanceSgs(instance.SecurityGroups)

					o.Expect(instanceFlavor.Name).To(o.Equal(flavorMachineSet), "Flavor not matching for instance %q", instance.Name)
					o.Expect(instanceImage.Name).To(o.Equal(imageMachineSet), "Image not matching for instance %q", instance.Name)
					o.Expect(instanceSgs).To(o.Equal(sgMachineSets), "SGs not matching for %q", instance.Name)
				}
			}
		})

		// https://bugzilla.redhat.com/show_bug.cgi?id=2022627
		g.It("should include the addresses on the machine specs", func() {

			skipUnlessMachineAPIOperator(dc, clientSet.CoreV1().Namespaces())
			g.By("fetching machines")
			machines, err := getMachines(dc)
			o.Expect(err).NotTo(o.HaveOccurred())

			for _, machine := range machines {
				g.By(fmt.Sprintf("Gather Openstack attributes for machine %q", machine.Get("metadata.name")))
				instance, err := servers.Get(computeClient, machine.Get("metadata.annotations.openstack-resourceId").String()).Extract()
				o.Expect(err).NotTo(o.HaveOccurred(), "Error gathering Openstack info for machine %v", machine.Get("metadata.name"))
				g.By(fmt.Sprintf("Compare addresses with openstack interfaces for machine %q", instance.Name))
				o.Expect(parseInstanceAddresses(instance.Addresses)).To(o.ConsistOf(getAddressesFromMachine(machine)), "Addresses not matching for instance %q", instance.Name)
			}
		})

	})
})

// listWorkerMachineSets lists all worker machineSets
func listWorkerMachineSets(dc dynamic.Interface) ([]objx.Map, error) {
	mc := dc.Resource(schema.GroupVersionResource{
		Group:    machineAPIGroup,
		Version:  "v1beta1",
		Resource: "machinesets",
	}).Namespace(machineAPINamespace)
	obj, err := mc.List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	machineSets := []objx.Map{}
	for _, ms := range objects(objx.Map(obj.UnstructuredContent()).Get("items")) {
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

// getMachines lists all machines in the cluster
func getMachines(dc dynamic.Interface) ([]objx.Map, error) {
	mc := dc.Resource(schema.GroupVersionResource{
		Group:    machineAPIGroup,
		Version:  "v1beta1",
		Resource: "machines",
	}).Namespace(machineAPINamespace)
	obj, err := mc.List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return objects(objx.Map(obj.UnstructuredContent()).Get("items")), nil
}

// getMachinesFromMachineSet lists all worker machines beloging to msName machineset
func getMachinesFromMachineSet(dc dynamic.Interface, msName string) ([]objx.Map, error) {
	machines, err := getMachines(dc)
	if err != nil {
		return nil, err
	}
	result := []objx.Map{}
	for _, machine := range machines {
		labels := (*machine.Get("metadata.labels")).Data().(map[string]interface{})
		if val, ok := labels[machineSetOwningLabel]; ok {
			if val == msName {
				result = append(result, machine)
				continue
			}
		}
	}
	return result, nil
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

// returns the list of securityGroups
func getSecurityGroupNames(item objx.Map) []string {
	listSg := objects(item.Get("spec.template.spec.providerSpec.value.securityGroups"))
	result := make([]string, 0, len(listSg))
	for _, sg := range listSg {
		result = append(result, fmt.Sprintf("%v", sg["name"]))
	}
	return result
}

// parse instance.SecurityGroup object and return a slice of unique
// security groups
func parseInstanceSgs(securityGroups []map[string]interface{}) []string {
	result := make([]string, 0, len(securityGroups))
	add := true

	for _, item := range securityGroups {
		for _, sg := range result {
			add = true
			if sg == fmt.Sprintf("%v", item["name"]) {
				add = false
			}
		}
		if add {
			result = append(result, fmt.Sprintf("%v", item["name"]))
		}
	}
	return result
}

// returns the list of addresses
func getAddressesFromMachine(item objx.Map) []string {
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
func parseInstanceAddresses(addresses map[string]interface{}) ([]string, error) {
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

// skipUnlessMachineAPI is used to determine if the Machine API is installed and running in a cluster.
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
