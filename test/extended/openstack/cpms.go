package openstack

import (
	"context"
	"fmt"
	"sort"
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

	var ctx context.Context
	var dc dynamic.Interface
	var clientSet *kubernetes.Clientset
	var controlPlaneMachineSet objx.Map
	var controlPlaneMachines []objx.Map
	var numCpms int

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
		controlPlaneMachineSet, numCpms, err = getControlPlaneMachineSet(ctx, dc)
		if err != nil {
			if numCpms == 0 {
				// In some cases (e.g. UPI), the control plane machine set is not created.
				e2eskipper.Skipf("Failed to get control plane machine sets: %v", err)
			} else {
				o.Expect(err).NotTo(o.HaveOccurred())
			}
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
		var machineNovaAvailabilityZones []string
		var cpmsNovaAvailabilityZones []string

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
				nova_az := fd.Get("availabilityZone").String()
				if nova_az != "" {
					cpmsNovaAvailabilityZones = append(cpmsNovaAvailabilityZones, nova_az)
				}
			}
		}
		sort.Strings(cpmsNovaAvailabilityZones)

		for _, machine := range controlPlaneMachines {
			machineName := machine.Get("metadata.name").String()
			g.By("Comparing the MachineSet spec with machine " + machineName)

			machineFlavor := machine.Get("spec.providerSpec.value.flavor").String()
			machineImage := machine.Get("spec.providerSpec.value.image").String()
			machineSecurityGroups := make(map[string]struct{})
			machineSubnets := make(map[string]struct{})
			machineServerGroup := machine.Get("spec.providerSpec.value.serverGroupName").String()
			machineNovaAvailabilityZone := machine.Get("spec.providerSpec.value.availabilityZone").String()

			for _, sg := range objects(machine.Get("spec.providerSpec.value.securityGroups")) {
				securityGroupName := sg.Get("name").String()
				machineSecurityGroups[securityGroupName] = struct{}{}
			}

			for _, subnet := range objects(machine.Get("spec.providerSpec.value.networks[0].subnets")) {
				subnetName := subnet.Get("filter.name").String()
				machineSubnets[subnetName] = struct{}{}
			}
			if machineNovaAvailabilityZone != "" {
				machineNovaAvailabilityZones = append(machineNovaAvailabilityZones, machineNovaAvailabilityZone)
			}
			o.Expect(machineFlavor).To(o.Equal(cpmsFlavor), "flavor mismatch on Machine %q", machineName)
			o.Expect(machineImage).To(o.Equal(cpmsImage), "image mismatch on Machine %q", machineName)
			o.Expect(machineSecurityGroups).To(o.Equal(cpmsSecurityGroups), "security group mismatch on Machine %q", machineName)
			o.Expect(machineSubnets).To(o.Equal(cpmsSubnets), "subnets mismatch on Machine %q", machineName)
			o.Expect(machineServerGroup).To(o.Equal(cpmsServerGroup), "server group	 mismatch on Machine %q", machineName)
		}
		sort.Strings(machineNovaAvailabilityZones)
		o.Expect(machineNovaAvailabilityZones).To(o.Equal(cpmsNovaAvailabilityZones))
	})
})

func getControlPlaneMachineSet(ctx context.Context, dc dynamic.Interface) (objx.Map, int, error) {
	var numCpms int = 0
	mc := dc.Resource(schema.GroupVersionResource{
		Group:    machineAPIGroup,
		Version:  "v1",
		Resource: "controlplanemachinesets",
	}).Namespace(machineAPINamespace)
	obj, err := mc.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, numCpms, err
	}
	cpmsList := objx.Map(obj.UnstructuredContent()).Get("items").ObjxMapSlice()
	if numCpms := len(cpmsList); numCpms != 1 {
		return nil, numCpms, fmt.Errorf("expected one CPMS, found %d", numCpms)
	}
	return cpmsList[0], numCpms, nil
}
