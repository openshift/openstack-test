package openstack

import (
	"context"
	"strconv"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	"github.com/stretchr/objx"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

const (
	machineAPINamespace   = "openshift-machine-api"
	machineLabelRole      = "machine.openshift.io/cluster-api-machine-role"
	machineAPIGroup       = "machine.openshift.io"
	machineSetOwningLabel = "machine.openshift.io/cluster-api-machineset"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] MachineSet", func() {

	// getMachinesByMachineSet returns all machines beloging to the MachineSet with the given name
	getMachinesByMachineSet := func(ctx context.Context, dc dynamic.Interface, machineSetName string) ([]objx.Map, error) {
		machines, err := getMachines(ctx, dc)
		if err != nil {
			return nil, err
		}
		result := make([]objx.Map, 0, len(machines))
		for i := range machines {
			labels := machines[i].Get("metadata.labels").Data().(map[string]interface{})
			if val, ok := labels[machineSetOwningLabel]; ok {
				if val == machineSetName {
					result = append(result, machines[i])
				}
			}
		}
		return result, nil
	}

	defer g.GinkgoRecover()

	var ctx context.Context
	var dc dynamic.Interface
	var clientSet *kubernetes.Clientset
	var machineSets []objx.Map

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

		machineSets, err = getMachineSets(ctx, dc)
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It("have role worker", func() {
		for i := range machineSets {
			labels := machineSets[i].Get("spec.template.metadata.labels").Data().(map[string]interface{})
			o.Expect(labels[machineLabelRole]).To(o.Equal("worker"), "unexpected or absent MachineSet role label")
		}
	})

	g.It("replica number corresponds to the number of Machines", func() {
		for _, machineSet := range machineSets {
			machineSetName := machineSet.Get("metadata.name").String()
			replicaNumber, _ := strconv.Atoi(machineSet.Get("spec.replicas").String())
			o.Expect(getMachinesByMachineSet(ctx, dc, machineSetName)).
				To(o.HaveLen(replicaNumber), "unexpected number of replicas for machineset %q", machineSetName)
		}
	})

	g.It("ProviderSpec template is correctly applied to Machines", func() {
		for _, machineSet := range machineSets {
			msName := machineSet.Get("metadata.name").String()

			msFlavor := machineSet.Get("spec.template.spec.providerSpec.value.flavor").String()
			msImage := machineSet.Get("spec.template.spec.providerSpec.value.image").String()
			msSecurityGroups := make(map[string]struct{})
			for _, sg := range objects(machineSet.Get("spec.template.spec.providerSpec.value.securityGroups")) {
				securityGroupName := sg["name"].(string)
				msSecurityGroups[securityGroupName] = struct{}{}
			}

			machines, err := getMachinesByMachineSet(ctx, dc, msName)
			o.Expect(err).NotTo(o.HaveOccurred(), "error fetching machines for MachineSet %q", msName)

			for _, machine := range machines {
				machineName := machine.Get("metadata.name").String()
				g.By("Comparing the MachineSet spec with machine " + machineName)

				machineFlavor := machine.Get("spec.providerSpec.value.flavor").String()
				machineImage := machine.Get("spec.providerSpec.value.image").String()
				machineSecurityGroups := make(map[string]struct{})
				for _, sg := range objects(machine.Get("spec.providerSpec.value.securityGroups")) {
					securityGroupName := sg["name"].(string)
					machineSecurityGroups[securityGroupName] = struct{}{}
				}

				o.Expect(machineFlavor).To(o.Equal(msFlavor), "flavor mismatch on Machine %q of MachineSet %q", machineName, msName)
				o.Expect(machineImage).To(o.Equal(msImage), "image mismatch on Machine %q of MachineSet %q", machineName, msName)
				o.Expect(machineSecurityGroups).To(o.Equal(msSecurityGroups), "security group mismatch on Machine %q of MachineSet %q", machineName, msName)
			}
		}
	})
})

// getMachineSets returns all the available MachineSets
func getMachineSets(ctx context.Context, dc dynamic.Interface) ([]objx.Map, error) {
	mc := dc.Resource(schema.GroupVersionResource{
		Group:    machineAPIGroup,
		Version:  "v1beta1",
		Resource: "machinesets",
	}).Namespace(machineAPINamespace)
	obj, err := mc.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return objects(objx.Map(obj.UnstructuredContent()).Get("items")), nil
}
