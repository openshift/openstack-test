package openstack

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/openstack/sharedfilesystems/v2/shares"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/stretchr/objx"
	yaml "gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2edeployment "k8s.io/kubernetes/test/e2e/framework/deployment"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] The OpenStack platform", func() {
	defer g.GinkgoRecover()

	var dc dynamic.Interface
	var clientSet *kubernetes.Clientset
	var volumeClient *gophercloud.ServiceClient
	var shareClient *gophercloud.ServiceClient
	oc := exutil.NewCLI("openstack")

	var ctx context.Context
	g.BeforeEach(func() {
		ctx = context.Background()
	})

	g.Context("on volume creation", func() {

		g.BeforeEach(func(ctx g.SpecContext) {
			g.By("preparing openshift dynamic client")
			cfg, err := e2e.LoadConfig()
			o.Expect(err).NotTo(o.HaveOccurred())
			dc, err = dynamic.NewForConfig(cfg)
			o.Expect(err).NotTo(o.HaveOccurred())
			clientSet, err = e2e.LoadClientset()
			o.Expect(err).NotTo(o.HaveOccurred())
			g.By("preparing openstack client")
			volumeClient, err = client("volume")
			o.Expect(err).NotTo(o.HaveOccurred())
		})

		// https://access.redhat.com/support/cases/#/case/03081641
		// https://access.redhat.com/solutions/5325711
		g.It("should follow PVC specs during resizing for prometheus", func() {

			if !isPersistentStorageEnabledOnPrometheusK8s(ctx, clientSet) {
				e2eskipper.Skipf("openshift-monitoring does not have Persistent Storage enabled.")
			}

			g.By("Gather prometheus PVCs before resizing")
			initial_pvcs, err := getMonitoringPvcs(ctx, dc)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Gather Openstack cinder volumes for the PVCs before resizing")
			var initial_volumes []volumes.Volume
			for _, pvc := range initial_pvcs {
				cinderVolumes, err := getVolumesFromName(volumeClient, pvc.Get("spec.volumeName").String())
				o.Expect(err).NotTo(o.HaveOccurred(), "Error gathering Openstack info for PVC %q", pvc.Get("metadata.name"))
				o.Expect(cinderVolumes).To(o.HaveLen(1), "unexpected number of volumes for %q", pvc.Get("metadata.name"))
				initial_volumes = append(initial_volumes, cinderVolumes[0])
			}

			g.By("Checking size consistency before resizing")
			err = checkSizeConsistency(initial_pvcs, initial_volumes)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Resize PVCs increasing by 1Gi")
			prometheus_schema := schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheuses"}
			prometheus_interface := dc.Resource(prometheus_schema).Namespace("openshift-monitoring")
			statefulset_schema := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
			stateful_interface := dc.Resource(statefulset_schema).Namespace("openshift-monitoring")
			pvc_schema := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}
			pvc_interface := dc.Resource(pvc_schema).Namespace("openshift-monitoring")

			//oc patch --type=merge --patch='{"spec":{"paused":true}}' Prometheus/k8s -n openshift-monitoring
			_, err = prometheus_interface.Patch(ctx, "k8s",
				types.MergePatchType, []byte(`{"spec": {"paused": true}}`), metav1.PatchOptions{})
			o.Expect(err).NotTo(o.HaveOccurred(), "failure pausing prometheus")

			//oc scale statefulset.apps/prometheus-k8s -n openshift-monitoring --replicas=0
			_, err = stateful_interface.Patch(ctx, "prometheus-k8s",
				types.MergePatchType, []byte(`{"spec": {"replicas": 0}}`), metav1.PatchOptions{})
			o.Expect(err).NotTo(o.HaveOccurred(), "failure scaling down prometheus-k8s")

			//oc edit pvc prometheus-k8s-db-prometheus-k8s-0 -n openshift-monitoring
			for _, pvc := range initial_pvcs {
				vols, err := getVolumesFromName(volumeClient, pvc.Get("spec.volumeName").String())
				o.Expect(err).NotTo(o.HaveOccurred(), "Error gathering Openstack info for PVC %q", pvc.Get("metadata.name"))
				o.Expect(vols).To(o.HaveLen(1), "unexpected number of volumes for %q", pvc.Get("metadata.name"))
				new_size := vols[0].Size + 1
				resize_spec := []byte(fmt.Sprintf(`{"spec": {"resources": {"requests":{"storage": "%dGi"}}}}`, new_size))
				_, err = pvc_interface.Patch(ctx, pvc.Get("metadata.name").String(),
					types.MergePatchType, resize_spec, metav1.PatchOptions{})
				o.Expect(err).NotTo(o.HaveOccurred(), "failure changing storage size on the PVC")
			}

			//oc patch --type=merge --patch='{"spec":{"paused":false}}' Prometheus/k8s -n openshift-monitoring
			_, err = prometheus_interface.Patch(ctx, "k8s",
				types.MergePatchType, []byte(`{"spec": {"paused": false}}`), metav1.PatchOptions{})
			o.Expect(err).NotTo(o.HaveOccurred(), "failure resuming prometheus")

			//oc scale statefulset.apps/prometheus-k8s -n openshift-monitoring --replicas=<number of existing pvcs
			replicas_spec := []byte(fmt.Sprintf(`{"spec": {"replicas": %d}}`, len(initial_pvcs)))
			_, err = stateful_interface.Patch(ctx, "prometheus-k8s",
				types.MergePatchType, replicas_spec, metav1.PatchOptions{})
			o.Expect(err).NotTo(o.HaveOccurred(), "failure scaling up prometheus-k8s")
			time.Sleep(5 * time.Second) // Give time to the cluster to apply the changes

			g.By("Wait until prometheus-k8s pods are ready again")
			_, err = exutil.WaitForPods(clientSet.CoreV1().Pods("openshift-monitoring"),
				exutil.ParseLabelsOrDie("prometheus=k8s"), exutil.CheckPodIsRunning, 2, 3*time.Minute)
			o.Expect(err).NotTo(o.HaveOccurred(), "timeout waiting for prometheus=k8s pods going to running state after the resize")

			g.By("Active wait checking that the status after resizing is expected (max 1 minute)")
			o.Eventually(func() error {
				e2e.Logf("Gather prometheus PVCs after resizing")
				resized_pvcs, err := getMonitoringPvcs(ctx, dc)
				if err != nil {
					return err
				}
				if len(resized_pvcs) != len(initial_pvcs) {
					return fmt.Errorf("unexpected number of PVCs after resizing %d", len(resized_pvcs))
				}

				e2e.Logf("Gather Openstack cinder volumes for the PVCs after resizing")
				var resized_volumes []volumes.Volume
				for _, pvc := range resized_pvcs {
					cinderVolumes, err := getVolumesFromName(volumeClient, pvc.Get("spec.volumeName").String())
					if err != nil {
						return fmt.Errorf("error gathering Openstack info for PVC %q", pvc.Get("metadata.name"))
					}
					if len(cinderVolumes) != 1 {
						return fmt.Errorf("unexpected number of volumes for %q: %d", pvc.Get("metadata.name"), len(cinderVolumes))
					}
					resized_volumes = append(resized_volumes, cinderVolumes[0])
				}

				e2e.Logf("Checking size consistency after resizing")
				err = checkSizeConsistency(resized_pvcs, resized_volumes)
				if err != nil {
					return err
				}
				if len(resized_volumes) != len(initial_volumes) {
					return fmt.Errorf("unexpected number of cinder volumes after resizing: %d", len(resized_volumes))
				}

				e2e.Logf("Check cinder volumes status after resizing")
				for _, initvol := range initial_volumes {
					found := false
					for _, rszvol := range resized_volumes {
						if !found && initvol.Name == rszvol.Name {
							found = true
							if initvol.Size+1 != rszvol.Size {
								return fmt.Errorf("Unexpected size on resized volume: %d", initvol.Size+1)
							}
							if rszvol.Status != "in-use" {
								return fmt.Errorf("cinder volume not in-use Status")
							}
							e2e.Logf("Cinder Volume '%q' has been successfully resized from %d to %d",
								initvol.Name, initvol.Size, rszvol.Size)

						}
					}
					if found != true {
						return fmt.Errorf("Pre-existing cinder volume %q is gone.", initvol.Name)
					}
				}
				return nil
			}, "60s", "10s").Should(o.BeNil())
		})

		g.It("should create a manila share when using manila storage class", func(ctx g.SpecContext) {
			var err error
			manilaSc := FindStorageClassByProvider(oc, "manila.csi.openstack.org")

			// Skip if Manila Storage class is not defined
			if manilaSc == nil {
				e2eskipper.Skipf("No StorageClass with manila.csi.openstack.org provisioner")
			}

			ns := oc.Namespace()
			pvc := CreatePVC(ctx, clientSet, "manila-pvc", ns, manilaSc.Name, "1Gi")
			fileContent := "hello"

			shareClient, err = client("sharev2")
			o.Expect(err).NotTo(o.HaveOccurred())
			// Make sure a Manila share was created with the same name as the PVC
			err = waitPvcVolume(ctx, clientSet, pvc.Name, ns)
			o.Expect(err).NotTo(o.HaveOccurred())
			manilaShares, err := GetSharesFromName(shareClient, pvc.Spec.VolumeName)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(manilaShares)).To(o.Equal(1))
			shareID := manilaShares[0].ID
			_, err = shares.Get(shareClient, shareID).Extract()
			o.Expect(err).NotTo(o.HaveOccurred())

			// Creating a deployment with the volumes attached
			g.By("Creating Openshift deployment with 2 replicas and volumes attached")
			labels := map[string]string{"app": "manila-test-dep"}

			testDeployment := createTestDeployment(deploymentOpts{
				Name:     "manila-test-dep",
				Labels:   labels,
				Replicas: 2,
				Protocol: v1.ProtocolTCP,
				Port:     8080,
				Volumes: []volumeOption{volumeOption{
					Name:      "data-volume",
					PvcName:   pvc.Name,
					MountPath: "data",
				}},
			})

			deployment, err := clientSet.AppsV1().Deployments(ns).Create(ctx,
				testDeployment, metav1.CreateOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			err = e2edeployment.WaitForDeploymentComplete(clientSet, deployment)
			o.Expect(err).NotTo(o.HaveOccurred())
			pods, err := oc.AdminKubeClient().CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())

			// Create a file in one pod's attached volume and read it in the second one
			_, err = oc.Run("exec").Args(pods.Items[0].Name, "--", "bash", "-c", fmt.Sprintf("echo %s > /data/1", fileContent)).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			out, err := oc.Run("exec").Args(pods.Items[1].Name, "--", "bash", "-c", "cat /data/1").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(out).To(o.Equal(fileContent))
		})
	})
})

// Compare sizes of PVCs and Cinder volumes with the same Name. Return error if any.
func checkSizeConsistency(pvcs []objx.Map, volumes []volumes.Volume) error {
	for _, pvc := range pvcs {
		pvcSize, err := strconv.Atoi(strings.Split(pvc.Get("status.capacity.storage").String(), "Gi")[0])
		if err != nil {
			return fmt.Errorf("error converting PVC size into integer for %q: %q",
				pvc.Get("metadata.name"), pvc.Get("status.capacity.storage"))
		}
		found := false
		for _, vol := range volumes {
			if !found && vol.Name == pvc.Get("spec.volumeName").String() {
				found = true
				if vol.Size != pvcSize {
					return fmt.Errorf("sizes not matching for PVC %q. volume size: %v VS pvc size: %v",
						pvc.Get("metadata.name"), vol.Size, pvcSize)
				}
			}
		}
		if !found {
			return fmt.Errorf("corresponding Cinder Volume for PVC %q not found", pvc.Get("metadata.name"))
		}
	}
	return nil
}

// return list of PVCs defined in openshift-monitoring namespace
func getMonitoringPvcs(ctx context.Context, dc dynamic.Interface) ([]objx.Map, error) {
	pvc_schema := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}
	obj, err := dc.Resource(pvc_schema).Namespace("openshift-monitoring").
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return objects(objx.Map(obj.UnstructuredContent()).Get("items")), nil
}

// return volume from openstack with specific name
func getVolumesFromName(client *gophercloud.ServiceClient, volumeName string) ([]volumes.Volume, error) {
	var emptyVol []volumes.Volume
	listOpts := volumes.ListOpts{
		Name: volumeName,
	}
	allPages, err := volumes.List(client, listOpts).AllPages()
	if err != nil {
		return emptyVol, err
	}
	volumes, err := volumes.ExtractVolumes(allPages)
	if err != nil {
		return emptyVol, err
	}
	return volumes, nil
}

func isPersistentStorageEnabledOnPrometheusK8s(ctx context.Context, kubeClient kubernetes.Interface) bool {
	cmClient := kubeClient.CoreV1().ConfigMaps("openshift-monitoring")
	config, err := cmClient.Get(ctx, "cluster-monitoring-config", metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return false
	}

	var configData map[string]map[string]interface{}
	err = yaml.Unmarshal([]byte(config.Data["config.yaml"]), &configData)
	if err != nil {
		return false
	}

	_, found := configData["prometheusK8s"]["volumeClaimTemplate"]
	return found
}
