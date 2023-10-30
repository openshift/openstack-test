package openstack

import (
	"context"
	"fmt"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	"github.com/openshift/openstack-test/test/extended/openstack/machines"
	"github.com/stretchr/objx"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/images"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] Machine", func() {
	defer g.GinkgoRecover()

	var ctx context.Context
	var dc dynamic.Interface
	var clientSet *kubernetes.Clientset
	var computeClient *gophercloud.ServiceClient
	var machineResources []objx.Map

	g.BeforeEach(func() {
		ctx = context.Background()

		g.By("preparing openshift dynamic client")
		cfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		dc, err = dynamic.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		skipUnlessMachineAPIOperator(ctx, dc, clientSet.CoreV1().Namespaces())

		g.By("preparing openstack client")
		computeClient, err = client(serviceCompute)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("fetching Machines")
		machineResources, err = machines.List(ctx, dc)
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It("are in phase Running", func() {
		for _, machine := range machineResources {
			o.Expect(machine.Get("status.phase").String()).To(o.Equal("Running"), "unexpected phase for Machine %q", machine.Get("metadata.name"))
		}
	})

	g.It("ProviderSpec is correctly applied to OpenStack instances", func() {
		for _, machine := range machineResources {
			machineName := machine.Get("metadata.name").String()
			machineFlavor := machine.Get("spec.providerSpec.value.flavor").String()
			machineImage := machine.Get("spec.providerSpec.value.image").String()
			machineSecurityGroups := make(map[string]struct{})
			for _, sg := range objects(machine.Get("spec.providerSpec.value.securityGroups")) {
				securityGroupName := sg["name"].(string)
				machineSecurityGroups[securityGroupName] = struct{}{}
			}

			g.By(fmt.Sprintf("Gathering Openstack instance for Machine %q", machineName))
			instance, err := servers.Get(computeClient, machine.Get("metadata.annotations.openstack-resourceId").String()).Extract()
			o.Expect(err).NotTo(o.HaveOccurred(), "Error fetching Openstack instance for Machine %q", machineName)

			g.By(fmt.Sprintf("Comparing Machine %q with instance %q: flavor", machineName, instance.Name), func() {
				instanceFlavorID := instance.Flavor["id"].(string)
				instanceFlavor, err := flavors.Get(computeClient, instanceFlavorID).Extract()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(instanceFlavor.Name).To(o.Equal(machineFlavor), "Flavor not matching for instance %q", instance.Name)
			})

			g.By(fmt.Sprintf("Comparing Machine %q with instance %q: image", machineName, instance.Name), func() {
				// Instance doesn't reference an image when using root volumes
				if instance.Image["id"] != nil {
					instanceImage, err := images.Get(computeClient, fmt.Sprintf("%v", instance.Image["id"])).Extract()
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(instanceImage.Name).To(o.Equal(machineImage), "Image not matching for instance %q", instance.Name)
				}
			})

			g.By(fmt.Sprintf("Comparing Machine %q with instance %q: root volume", machineName, instance.Name), func() {
				if instance.Image["id"] != nil {
					instanceImage, err := images.Get(computeClient, fmt.Sprintf("%v", instance.Image["id"])).Extract()
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(instanceImage.Name).To(o.Equal(machineImage), "Image not matching for instance %q", instance.Name)
				}
			})

			g.By(fmt.Sprintf("Comparing Machine %q with instance %q: security groups", machineName, instance.Name), func() {
				instanceSecurityGroups := make(map[string]struct{})
				for i := range instance.SecurityGroups {
					securityGroupName := instance.SecurityGroups[i]["name"].(string)
					instanceSecurityGroups[securityGroupName] = struct{}{}
				}

				o.Expect(instanceSecurityGroups).To(o.Equal(machineSecurityGroups), "SGs not matching for %q", instance.Name)
			})
		}

	})
})

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

// skipUnlessMachineAPI is used to determine if the Machine API is installed and running in a cluster.
// It is expected to skip the test if it determines that the Machine API is not installed/running.
// Use this early in a test that relies on Machine API functionality.
//
// It checks to see if the machine custom resource is installed in the cluster.
// If machines are not installed it skips the test case.
// It then checks to see if the `openshift-machine-api` namespace is installed.
// If the namespace is not present it skips the test case.
func skipUnlessMachineAPIOperator(ctx context.Context, dc dynamic.Interface, c coreclient.NamespaceInterface) {
	machineClient := dc.Resource(schema.GroupVersionResource{Group: "machine.openshift.io", Resource: "machines", Version: "v1beta1"})

	err := wait.PollImmediate(time.Second, time.Minute, func() (bool, error) {
		// Listing the resource will return an IsNotFound error when the CRD has not been installed.
		// Otherwise it would return an empty list.
		_, err := machineClient.List(ctx, metav1.ListOptions{})
		if err == nil {
			return true, nil
		}
		if apierrors.IsNotFound(err) {
			e2eskipper.Skipf("The cluster does not support machine instances")
		}
		e2e.Logf("Unable to check for machine api operator: %v", err)
		return false, nil
	})
	o.Expect(err).NotTo(o.HaveOccurred())

	err = wait.PollImmediate(time.Second, time.Minute, func() (bool, error) {
		_, err := c.Get(ctx, "openshift-machine-api", metav1.GetOptions{})
		if err == nil {
			return true, nil
		}
		if apierrors.IsNotFound(err) {
			e2eskipper.Skipf("The cluster machines are not managed by machine api operator")
		}
		e2e.Logf("Unable to check for machine api operator: %v", err)
		return false, nil
	})
	o.Expect(err).NotTo(o.HaveOccurred())
}

func mapKeys[K comparable, V any](m map[K]V) (keys []K) {
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
