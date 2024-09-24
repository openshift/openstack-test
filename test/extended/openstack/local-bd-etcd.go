package openstack

import (
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	mcv1client "github.com/openshift/client-go/machineconfiguration/clientset/versioned"
	"github.com/openshift/openstack-test/test/extended/openstack/client"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/stretchr/objx"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] The OpenShift cluster", func() {
	defer g.GinkgoRecover()

	const minEtcdDiskSizeGiB = 10
	const blockDevicePath = "/dev/vdb"

	oc := exutil.NewCLI("openstack")
	var computeClient *gophercloud.ServiceClient
	var dc dynamic.Interface
	var mcClient *mcv1client.Clientset
	var controlPlaneFlavor string
	var clientSet *kubernetes.Clientset
	var cpmsProviderSpec objx.Map
	var masterNodeList *v1.NodeList

	skipUnlessMachineConfigIsPresent := func(mcName string, ctx g.SpecContext) {
		_, err := mcClient.MachineconfigurationV1().MachineConfigs().Get(ctx, mcName, metav1.GetOptions{})
		if err != nil {
			e2eskipper.Skipf("The expected machineConfig with name %s is not installed - Skipping. Error msg: %v", mcName, err)
		}
	}

	g.BeforeEach(func(ctx g.SpecContext) {
		g.By("preparing a dynamic client")
		cfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		dc, err = dynamic.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())
		mcClient, err = mcv1client.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())

		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("preparing the openstack client")
		computeClient, err = client.GetServiceClient(ctx, openstack.NewComputeV2)
		o.Expect(err).NotTo(o.HaveOccurred())

		skipUnlessMachineAPIOperator(ctx, dc, clientSet.CoreV1().Namespaces())
		skipUnlessMachineConfigIsPresent("98-var-lib-etcd", ctx)

		cpms, err := getControlPlaneMachineSet(ctx, dc)
		if err != nil {
			e2eskipper.Skipf("Failed to get control plane machine sets: %v", err)
		}
		cpmsProviderSpec = cpms.Get("spec.template.machines_v1beta1_machine_openshift_io.spec.providerSpec").ObjxMap()

		masterNodeList, err = clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/master",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
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
		g.By("checking that the disk is succesfully mounted on the masters")
		for _, item := range masterNodeList.Items {
			out, err := oc.SetNamespace("default").AsAdmin().Run("debug").Args("node/"+item.Name,
				"--", "chroot", "/host", "lsblk", blockDevicePath).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(out).To(o.ContainSubstring("/var/lib/etcd"),
				"%s: Ephemeral disk is not mounted on 'var/lib/etcd' as expected:\n%q\n", item.Name, out)
		}
	})
})
