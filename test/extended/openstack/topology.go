package openstack

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/openshift/openstack-test/test/extended/openstack/client"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	vavailabilityzones "github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/availabilityzones"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	cavailabilityzones "github.com/gophercloud/gophercloud/v2/openstack/compute/v2/availabilityzones"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	testutils "k8s.io/kubernetes/test/utils"

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

	g.It("should allow the manual setting of enable_topology to false [Serial]", func(ctx g.SpecContext) {

		// Get the enable_topology param from the openshift-cluster-csi-drivers cloud-conf
		g.By("Getting the enable_topology value from cloud-conf ConfigMap from the CSI driver")
		enable_topology := getConfigValue(ctx,
			oc.AdminKubeClient(),
			"openshift-cluster-csi-drivers",
			"cloud-conf",
			"enable_topology",
		)

		e2e.Logf("enable_topology: '%v'", enable_topology)

		// Skip the test if enable_topology is not true
		if enable_topology != "true" {
			e2eskipper.Skipf("enable_topology needs to be true for this test to run")
		}

		// Set enable_topology value to false in the cloud provider config
		g.By("Setting the enable_topology value to false")
		err := setEnableTopologyKey(ctx, oc.AdminKubeClient(), false)
		o.Expect(err).NotTo(o.HaveOccurred(), "Error setting the enable_topology param in the cloud-provider-config")

		// Set the defer cleanup function for reverting the enable_topology change
		g.DeferCleanup(func() {
			ctx := context.Background()
			enableTopologyCleanup(ctx, oc)
		})

		// Wait until enable_topology is changed in openshift-cluster-csi-drivers cloud-conf
		g.By("Waiting until enable_topology is changed in the csi driver")
		waitUntilEnableTopologyValue(ctx, oc, false)
		e2e.Logf("enable_topology successfully changed in the csi driver")

		// Wait until the openstack-cinder-csi-driver-node daemonset is redeployed
		g.By("Waiting until the openstack-cinder-csi-driver-node daemonset is fully deployed and healthy")
		waitUntilDaemonsetReady(ctx, oc, "openshift-cluster-csi-drivers", "openstack-cinder-csi-driver-node")
		e2e.Logf("The openstack-cinder-csi-driver-node daemonset is fully deployed and healthy")

		// Look for the default cinder-csi storage class
		cinderSc := FindStorageClassByProvider(oc, "cinder.csi.openstack.org", true)
		o.Expect(cinderSc).NotTo(o.BeNil(), "default cinder-csi storageClass not found.")

		// Set the openstack volume client
		g.By("Setting the openstack volume client")
		volumeClient, err := client.GetServiceClient(ctx, openstack.NewBlockStorageV3)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to build the OpenStack client")

		// Create a PVC
		g.By("Creating a PVC")
		ns := oc.Namespace()
		pvc := CreatePVC(ctx, clientSet, "cinder-pvc", ns, cinderSc.Name, "1Gi")

		// Create a deployment with the volume attached
		g.By("Creating Openshift deployment with 1 replica and cinder volume attached")
		labels := map[string]string{"app": "cinder-test-dep"}

		testDeployment := createTestDeployment(deploymentOpts{
			Name:     "cinder-test-dep",
			Labels:   labels,
			Replicas: 1,
			Protocol: v1.ProtocolTCP,
			Port:     8080,
			Volumes: []volumeOption{{
				Name:      "data-volume",
				PvcName:   pvc.Name,
				MountPath: "data",
			}},
		})

		deployment, err := clientSet.AppsV1().Deployments(ns).Create(ctx,
			testDeployment, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		err = testutils.WaitForDeploymentComplete(clientSet, deployment, e2e.Logf, 20*time.Second, 15*time.Minute)
		o.Expect(err).NotTo(o.HaveOccurred())

		// Get the PVCs from the test namespace
		pvcs, err := GetPVCsFromNamespace(ctx, dc, ns)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(pvcs)).To(o.Equal(1))

		// Check the volume in openstack
		cinderVolumes, err := getVolumesFromName(ctx, volumeClient, pvcs[0].Get("spec.volumeName").String())
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(cinderVolumes)).To(o.Equal(1))
		volumeID := cinderVolumes[0].ID
		_, err = volumes.Get(ctx, volumeClient, volumeID).Extract()
		o.Expect(err).NotTo(o.HaveOccurred())
	})
})

// Active wait until enable_topology has a specific value in the CSI driver
func waitUntilEnableTopologyValue(ctx context.Context, oc *exutil.CLI, value bool) {

	o.Eventually(func() string {
		enable_topology := getConfigValue(ctx,
			oc.AdminKubeClient(),
			"openshift-cluster-csi-drivers",
			"cloud-conf",
			"enable_topology",
		)

		e2e.Logf("Found enable_topology: '%v'", enable_topology)
		return enable_topology
	}, "30s", "1s").Should(o.Equal(strconv.FormatBool(value)),
		"Timed out waiting for enable_topology to have a value of '%s'", strconv.FormatBool(value))
}

// Active wait until a daemonset is fully deployed and healthy
func waitUntilDaemonsetReady(ctx context.Context, oc *exutil.CLI, namespace string, daemonSetName string) {

	o.Eventually(func() bool {

		ds, err := oc.AdminKubeClient().AppsV1().DaemonSets(namespace).Get(ctx, daemonSetName, metav1.GetOptions{})
		if err != nil {
			e2e.Logf("Failed to get DaemonSet '%s/%s': '%v'", namespace, daemonSetName, err)
			return false
		}

		status := ds.Status

		desired := status.DesiredNumberScheduled
		if desired == 0 {
			e2e.Logf("DaemonSet '%s/%s' contains '%v' desired daemon pods, retrying", namespace, daemonSetName, desired)
			return false
		}

		current := status.CurrentNumberScheduled
		ready := status.NumberReady
		upToDate := status.UpdatedNumberScheduled
		available := status.NumberAvailable
		e2e.Logf(
			"'%s' DaemonSet ("+
				"DESIRED:'%d', "+
				"CURRENT:'%d', "+
				"READY:'%d', "+
				"UP-TO-DATE:'%d', "+
				"AVAILABLE:'%d'"+
				")",
			daemonSetName, desired, current, ready, upToDate, available,
		)

		if current != desired ||
			ready != desired ||
			upToDate != desired ||
			available != desired {
			e2e.Logf("DaemonSet '%s/%s' not fully deployed and healthy, retrying", namespace, daemonSetName)
			return false
		}

		return true
	}, "30s", "5s").Should(o.BeTrue(),
		"Timed out waiting for the DaemonSet '%s/%s' to be fully deployed and healthy", namespace, daemonSetName)
}

// Set enable_topology value in the cloud-provider-config configmap
func setEnableTopologyKey(ctx context.Context, kubeClient kubernetes.Interface, value bool) error {

	cmClient := kubeClient.CoreV1().ConfigMaps("openshift-config")

	cm, err := cmClient.Get(ctx, "cloud-provider-config", metav1.GetOptions{})
	if err != nil {
		e2e.Logf("Failed to get configmap: : '%v'", err)
		return err
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}

	cm.Data["enable_topology"] = fmt.Sprintf("%t", value)

	_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		e2e.Logf("Failed to update configmap: : '%v'", err)
		return err
	}

	return nil
}

// Remove enable_topology key from the cloud-provider-config configmap
func removeEnableTopologyKey(ctx context.Context, kubeClient kubernetes.Interface) error {

	cmClient := kubeClient.CoreV1().ConfigMaps("openshift-config")

	cm, err := cmClient.Get(ctx, "cloud-provider-config", metav1.GetOptions{})
	if err != nil {
		e2e.Logf("Failed to get configmap: : '%v'", err)
		return err
	}

	if cm.Data != nil {
		if _, exists := cm.Data["enable_topology"]; exists {
			delete(cm.Data, "enable_topology")
		} else {
			e2e.Logf("enable_topology key does not exist in the configmap")
			return nil
		}
	}

	_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		e2e.Logf("Failed to update configmap after removing enable_topology: '%v'", err)
		return err
	}

	e2e.Logf("Successfully removed enable_topology key from the configmap")
	return nil
}

// Cleanup function for enable_topology setting in the cloud provider config
func enableTopologyCleanup(ctx context.Context, oc *exutil.CLI) {

	// Remove enable_topology value in the cloud provider config (by default it's auto-configured)
	g.By("Cleanup: Removing the enable_topology key from the cloud-provider-config configmap")
	err := removeEnableTopologyKey(ctx, oc.AdminKubeClient())
	o.Expect(err).NotTo(o.HaveOccurred(), "Cleanup: Error removing the enable_topology key from the cloud-provider-config configmap")
	e2e.Logf("Cleanup: enable_topology key successfully removed from the cloud-provider-config configmap")

	// Wait until enable_topology is changed in openshift-cluster-csi-drivers cloud-conf
	g.By("Cleanup: Waiting until enable_topology is changed in the csi driver")
	waitUntilEnableTopologyValue(ctx, oc, true)
	e2e.Logf("Cleanup: enable_topology successfully changed in the csi driver")

	// Wait until the openstack-cinder-csi-driver-node daemonset is redeployed
	g.By("Cleanup: Waiting until the openstack-cinder-csi-driver-node daemonset is fully deployed and healthy")
	waitUntilDaemonsetReady(ctx, oc, "openshift-cluster-csi-drivers", "openstack-cinder-csi-driver-node")
	e2e.Logf("Cleanup: the openstack-cinder-csi-driver-node daemonset is fully deployed and healthy")
}
