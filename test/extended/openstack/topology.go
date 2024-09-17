package openstack

import (
	"github.com/openshift/openstack-test/test/extended/openstack/client"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	vavailabilityzones "github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/availabilityzones"
	cavailabilityzones "github.com/gophercloud/gophercloud/v2/openstack/compute/v2/availabilityzones"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	exutil "github.com/openshift/origin/test/extended/util"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] The OpenShift cluster", func() {
	defer g.GinkgoRecover()

	var dc dynamic.Interface
	var clientSet *kubernetes.Clientset
	oc := exutil.NewCLI("openstack")

	g.BeforeEach(func(ctx g.SpecContext) {
		g.By("preparing a dynamic client")
		cfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		dc, err = dynamic.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		skipUnlessMachineAPIOperator(ctx, dc, clientSet.CoreV1().Namespaces())
	})
	// Automation for https://issues.redhat.com/browse/OCPBUGS-34792
	g.It("enables topology aware scheduling when compute and volume AZs are identical", func(ctx g.SpecContext) {
		var computeAZs []string
		var err error
		var volumeAZs []string
		var volumeClient *gophercloud.ServiceClient

		// Set compute and volume client
		computeClient, err := client.GetServiceClient(ctx, openstack.NewComputeV2)
		o.Expect(err).NotTo(o.HaveOccurred())
		volumeClient, err = client.GetServiceClient(ctx, openstack.NewBlockStorageV3)
		o.Expect(err).NotTo(o.HaveOccurred())

		// Get Openstack Compute availablility zones
		allPages, err := cavailabilityzones.List(computeClient).AllPages(ctx)
		o.Expect(err).NotTo(o.HaveOccurred())
		computeAzsInfo, err := cavailabilityzones.ExtractAvailabilityZones(allPages)
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, az := range computeAzsInfo {
			computeAZs = append(computeAZs, az.ZoneName)
		}

		// Get Openstack Volume availablility zones
		allPages, err = vavailabilityzones.List(volumeClient).AllPages(ctx)
		o.Expect(err).NotTo(o.HaveOccurred())
		volumeAzsInfo, err := vavailabilityzones.ExtractAvailabilityZones(allPages)
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, az := range volumeAzsInfo {
			volumeAZs = append(volumeAZs, az.ZoneName)
		}
		// Get enable_topology value from cloud-conf ConfigMap
		enable_topology := getConfigValue(ctx,
			oc.AdminKubeClient(),
			"openshift-cluster-csi-drivers",
			"cloud-conf",
			"enable_topology",
		)

		// If there are Compute AZs that are not in Volume AZs enable_topology should be false
		if len(difference(computeAZs, volumeAZs)) != 0 {
			o.Expect(enable_topology).To(o.Equal("false"))
		} else {
			o.Expect(enable_topology).To(o.Equal("true"))

		}
	})
})
