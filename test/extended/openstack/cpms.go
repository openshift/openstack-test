package openstack

import (
	"context"
	"fmt"
	"strconv"

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

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] ControlPlane MachineSet", func() {

	defer g.GinkgoRecover()

	var dc dynamic.Interface
	var clientSet *kubernetes.Clientset
	var controlPlaneMachineSet objx.Map
	var controlPlaneMachines []objx.Map

	g.BeforeEach(func(ctx g.SpecContext) {
		g.By("preparing openshift dynamic client")
		cfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		dc, err = dynamic.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		skipUnlessMachineAPIOperator(ctx, dc, clientSet.CoreV1().Namespaces())
		controlPlaneMachineSet, err = getControlPlaneMachineSet(ctx, dc)
		if err != nil {
			e2eskipper.Skipf("Failed to get control plane machine sets: %v", err)
		}
		controlPlaneMachines, err = machines.List(ctx, dc, machines.ByRole("master"))
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It("has role master", func() {
		labels := controlPlaneMachineSet.Get("spec.template.machines_v1beta1_machine_openshift_io.metadata.labels").ObjxMap()
		o.Expect(labels[machineLabelRole]).To(o.Equal("master"), "unexpected or absent role label")
	})

	g.It("replica number corresponds to the number of Machines", func() {
		replicaNumber, _ := strconv.Atoi(controlPlaneMachineSet.Get("spec.replicas").String())
		o.Expect(controlPlaneMachines).To(o.HaveLen(replicaNumber), "unexpected number of replicas for CPMS")
	})

	g.It("ProviderSpec template is correctly applied to Machines", func() {
		providerSpec := controlPlaneMachineSet.Get("spec.template.machines_v1beta1_machine_openshift_io.spec.providerSpec").ObjxMap()
		cpmsFlavor := providerSpec.Get("value.flavor").String()
		cpmsImage := providerSpec.Get("value.image").String()
		cpmsSecurityGroups := make(map[string]struct{})
		cpmsServerGroup := providerSpec.Get("value.serverGroupName").String()
		cpmsSubnets := make(map[string]struct{})
		var cpmsNetworks []string
		type FailureDomain struct {
			ComputeZone string
			VolumeZone  string
			VolumeType  string
		}
		var cpmsFDs []FailureDomain
		var machinesFDs []FailureDomain

		for _, network := range objects(providerSpec.Get("value.networks")) {
			subnet := network.Get("subnets").String()

			if subnet == "" {
				cpmsNetworks = append(cpmsNetworks, network.Get("uuid").String())
			}
		}

		for _, sg := range objects(providerSpec.Get("value.securityGroups")) {
			securityGroupName := sg.Get("name").String()
			cpmsSecurityGroups[securityGroupName] = struct{}{}
		}

		for _, subnet := range objects(providerSpec.Get("value.networks[0].subnets")) {
			subnetName := subnet.Get("filter.name").String()
			cpmsSubnets[subnetName] = struct{}{}
		}
		fds := controlPlaneMachineSet.Get("spec.template.machines_v1beta1_machine_openshift_io.failureDomains.openstack")

		if fds.String() != "" {
			for _, fd := range objects(controlPlaneMachineSet.Get("spec.template.machines_v1beta1_machine_openshift_io.failureDomains.openstack")) {
				cpmsFDs = append(cpmsFDs, FailureDomain{
					ComputeZone: fd.Get("availabilityZone").String(),
					VolumeZone:  fd.Get("rootVolume.availabilityZone").String(),
					VolumeType:  fd.Get("rootVolume.volumeType").String(),
				})
			}
		}
		for _, machine := range controlPlaneMachines {
			machineName := machine.Get("metadata.name").String()
			g.By("Comparing the MachineSet spec with machine " + machineName)

			machineProviderSpec := machine.Get("spec.providerSpec.value").ObjxMap()
			machineFlavor := machine.Get("spec.providerSpec.value.flavor").String()
			machineImage := machine.Get("spec.providerSpec.value.image").String()
			machineSecurityGroups := make(map[string]struct{})
			machineSubnets := make(map[string]struct{})
			machineServerGroup := machineProviderSpec.Get("serverGroupName").String()
			machineNovaAz := machine.Get("spec.providerSpec.value.availabilityZone").String()
			machineCinderAz := machine.Get("spec.providerSpec.value.rootVolume.availabilityZone").String()
			machineVolumeType := machine.Get("spec.providerSpec.value.rootVolume.volumeType").String()
			var machineNetworks []string

			for _, sg := range objects(machineProviderSpec.Get("securityGroups")) {
				securityGroupName := sg.Get("name").String()
				machineSecurityGroups[securityGroupName] = struct{}{}
			}

			for _, subnet := range objects(machine.Get("spec.providerSpec.value.networks[0].subnets")) {
				subnetName := subnet.Get("filter.name").String()
				machineSubnets[subnetName] = struct{}{}
			}
			if machineNovaAz != "" || machineCinderAz != "" || machineVolumeType != "" {
				fd := FailureDomain{
					ComputeZone: machineNovaAz,
					VolumeZone:  machineCinderAz,
					VolumeType:  machineVolumeType,
				}
				o.Expect(cpmsFDs).To(o.ContainElement(fd))
				machinesFDs = append(machinesFDs, fd)
			}

			for _, network := range objects(machine.Get("spec.providerSpec.value.networks")) {
				if network.Get("subnets").String() == "" {
					machineNetworks = append(machineNetworks, network.Get("uuid").String())
				}
			}

			o.Expect(machineFlavor).To(o.Equal(cpmsFlavor), "flavor mismatch on Machine %q", machineName)
			o.Expect(machineImage).To(o.Equal(cpmsImage), "image mismatch on Machine %q", machineName)
			o.Expect(machineSecurityGroups).To(o.Equal(cpmsSecurityGroups), "security group mismatch on Machine %q", machineName)
			o.Expect(machineSubnets).To(o.Equal(cpmsSubnets), "subnets mismatch on Machine %q", machineName)
			o.Expect(machineServerGroup).To(o.Equal(cpmsServerGroup), "server group	 mismatch on Machine %q", machineName)
			o.Expect(machineNetworks).To(o.Equal(cpmsNetworks), "Network mismatch on Machine %q", machineName)
		}
		if len(cpmsFDs) > 3 {
			o.Expect(machinesFDs).To(o.HaveLen(3))
		} else {
			o.Expect(machinesFDs).To(o.HaveLen(len(cpmsFDs)))
		}
	})
})

func getControlPlaneMachineSet(ctx context.Context, dc dynamic.Interface) (objx.Map, error) {
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
	return cpmsList[0], nil
}
