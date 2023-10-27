package openstack

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	"github.com/openshift/openstack-test/test/extended/openstack/machines"
	"github.com/stretchr/objx"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] The OpenShift cluster", func() {
	defer g.GinkgoRecover()

	const minEtcdDiskSizeGiB = 7
	const etcdBlockDeviceName = "etcd"

	var computeClient *gophercloud.ServiceClient
	var ctx context.Context
	var dc dynamic.Interface
	var controlPlaneFlavor string
	var clusterCPMS objx.Map
	var clientSet *kubernetes.Clientset

	checkEtcdDisk := func(machine objx.Map) error {
		additionalBlockDevices := machine.Get("spec.providerSpec.value.additionalBlockDevices").ObjxMapSlice()
		if len(additionalBlockDevices) == 0 {
			return fmt.Errorf("machine %q does not have additional block devices", machine.Get("metadata.name").String())
		}

		for _, blockDevice := range additionalBlockDevices {
			if blockDevice.Get("name").String() == etcdBlockDeviceName {
				if blockDevice.Get("sizeGiB").Int() < minEtcdDiskSizeGiB {
					return fmt.Errorf("machine %q has an additional block device with size %d GiB, which is less than the minimum required %d GiB", machine.Get("metadata.name").String(), blockDevice.Get("sizeGiB").Int(), minEtcdDiskSizeGiB)
				}
				if blockDevice.Get("storage.type").String() != "Local" {
					return fmt.Errorf("machine %q has an additional block device with storage type %q, which is not Local", machine.Get("metadata.name").String(), blockDevice.Get("storage.type").String())
				}
				return nil
			}
		}
		return fmt.Errorf("machine %q does not have an additional block device named %s", machine.Get("metadata.name").String(), etcdBlockDeviceName)
	}

	getControlPlaneMachineSets := func(ctx context.Context, dc dynamic.Interface) ([]objx.Map, error) {
		mc := dc.Resource(schema.GroupVersionResource{
			Group:    machineAPIGroup,
			Version:  "v1",
			Resource: "controlplanemachinesets",
		}).Namespace(machineAPINamespace)
		obj, err := mc.List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		cpmsList := objx.Map(obj.UnstructuredContent()).Get("items").ObjxMapSlice()
		if numCpms := len(cpmsList); numCpms != 1 {
			return nil, fmt.Errorf("expected one CPMS, found %d", numCpms)
		}
		return cpmsList, nil
	}

	skipUnlessEtcdAdditionalBlockDevice := func(ctx context.Context, dc dynamic.Interface) {
		controlPlaneMachineSets, err := getControlPlaneMachineSets(ctx, dc)
		o.Expect(err).NotTo(o.HaveOccurred())
		cpms := controlPlaneMachineSets[0]
		additionalBlockDevices := cpms.Get("spec.template.spec.providerSpec.value.additionalBlockDevices").ObjxMapSlice()
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

	g.BeforeEach(func() {
		ctx = context.Background()

		g.By("preparing a dynamic client")
		cfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		dc, err = dynamic.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		computeClient, err = client(serviceCompute)
		o.Expect(err).NotTo(o.HaveOccurred())

		skipUnlessMachineAPIOperator(ctx, dc, clientSet.CoreV1().Namespaces())
		skipUnlessEtcdAdditionalBlockDevice(ctx, dc)

	})

	g.It("runs with etcd on ephemeral local block device", func() {

		g.By("checking that the control plane flavor has enough ephemeral storage")
		{
			controlPlaneFlavor = clusterCPMS.Get("spec.template.spec.providerSpec.value.flavor").String()
			o.Expect(controlPlaneFlavor).NotTo(o.BeEmpty())
			allPages, err := flavors.ListDetail(computeClient, flavors.ListOpts{}).AllPages()
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
