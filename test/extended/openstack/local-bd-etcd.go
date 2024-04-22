package openstack

import (
	"fmt"
	"strconv"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	"github.com/openshift/openstack-test/test/extended/openstack/client"
	"github.com/openshift/openstack-test/test/extended/openstack/machines"
	"github.com/stretchr/objx"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] The OpenShift cluster", func() {
	defer g.GinkgoRecover()

	const minEtcdDiskSizeGiB = 10
	const etcdBlockDeviceName = "etcd"

	var computeClient *gophercloud.ServiceClient
	var dc dynamic.Interface
	var controlPlaneFlavor string
	var clientSet *kubernetes.Clientset
	var cpmsProviderSpec objx.Map

	checkEtcdDisk := func(machine objx.Map) error {
		additionalBlockDevices := machine.Get("spec.providerSpec.value.additionalBlockDevices").ObjxMapSlice()
		if len(additionalBlockDevices) == 0 {
			return fmt.Errorf("machine %q does not have additional block devices", machine.Get("metadata.name").String())
		}

		for _, blockDevice := range additionalBlockDevices {
			if blockDevice.Get("name").String() == etcdBlockDeviceName {
				sizeInt, _ := strconv.Atoi(blockDevice.Get("sizeGiB").String())
				o.Expect(sizeInt).To(o.BeNumerically(">=", minEtcdDiskSizeGiB))
				o.Expect(blockDevice.Get("storage.type").String()).To(o.Equal("Local"))
				return nil
			}
		}
		return fmt.Errorf("machine %q does not have an additional block device named %s", machine.Get("metadata.name").String(), etcdBlockDeviceName)
	}

	skipUnlessEtcdAdditionalBlockDevice := func(cpms objx.Map) {
		additionalBlockDevices := cpmsProviderSpec.Get("value.additionalBlockDevices").ObjxMapSlice()
		if len(additionalBlockDevices) == 0 {
			e2eskipper.Skipf("CPMS does not have additional block devices")
		}
		var etcdDiskFound bool
		for _, blockDevice := range additionalBlockDevices {
			if blockDevice.Get("name").String() == etcdBlockDeviceName {
				etcdDiskFound = true
			}
		}
		if !etcdDiskFound {
			e2eskipper.Skipf("CPMS does not have an additional block device named %s", etcdBlockDeviceName)
		}
	}

	g.BeforeEach(func(ctx g.SpecContext) {
		g.By("preparing a dynamic client")
		cfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		dc, err = dynamic.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("preparing the openstack client")
		computeClient, err = client.GetServiceClient(ctx, openstack.NewComputeV2)
		o.Expect(err).NotTo(o.HaveOccurred())

		skipUnlessMachineAPIOperator(ctx, dc, clientSet.CoreV1().Namespaces())

		cpms, err := getControlPlaneMachineSet(ctx, dc)
		if err != nil {
			e2eskipper.Skipf("Failed to get control plane machine sets: %v", err)
		}
		cpmsProviderSpec = cpms.Get("spec.template.machines_v1beta1_machine_openshift_io.spec.providerSpec").ObjxMap()

		skipUnlessEtcdAdditionalBlockDevice(cpms)
	})

	g.It("runs with etcd on ephemeral local block device", func(ctx g.SpecContext) {

		g.By("checking that the control plane flavor has enough ephemeral storage")
		{
			controlPlaneFlavor = cpmsProviderSpec.Get("value.flavor").String()
			o.Expect(controlPlaneFlavor).NotTo(o.BeEmpty())
			allPages, err := flavors.ListDetail(computeClient, flavors.ListOpts{}).AllPages(ctx)
			o.Expect(err).NotTo(o.HaveOccurred())
			flavors, err := flavors.ExtractFlavors(allPages)
			o.Expect(err).NotTo(o.HaveOccurred())
			for _, flavor := range flavors {
				if flavor.Name == controlPlaneFlavor {
					o.Expect(flavor.Ephemeral).To(o.BeNumerically(">=", minEtcdDiskSizeGiB))
					break
				}
			}
		}

		g.By("checking that control plane machines have the additional ephemeral disk for etcd")
		{
			controlPlaneMachines, err := machines.List(ctx, dc, machines.ByRole("master"))
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(controlPlaneMachines)).To(o.Equal(3))

			for _, cpMachine := range controlPlaneMachines {
				o.Expect(checkEtcdDisk(cpMachine)).To(o.Succeed())
			}
		}

	})
})
