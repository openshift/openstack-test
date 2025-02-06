package openstack

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	routev1 "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	"github.com/openshift/openstack-test/test/extended/openstack/client"
	exutil "github.com/openshift/origin/test/extended/util"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2edeployment "k8s.io/kubernetes/test/e2e/framework/deployment"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] Creating ScrapeConfig in rhoso", func() {
	defer g.GinkgoRecover()

	shiftstackKubeConfigEnvVarName := "KUBECONFIG"
	shiftstackPassFileEnvVarName := "SHIFTSTACK_PASS_FILE"
	rhosoKubeConfigEnvVarName := "RHOSO_KUBECONFIG"
	shiftstackKubeConfig := os.Getenv(shiftstackKubeConfigEnvVarName)
	rhosoKubeConfig := os.Getenv(rhosoKubeConfigEnvVarName)
	shiftstackPassFile := os.Getenv(shiftstackPassFileEnvVarName)
	openstackNamespaceName := "openstack"
	scrapeInterval := "30s"
	var computeClient *gophercloud.ServiceClient
	var volumeClient *gophercloud.ServiceClient
	var err error

	oc := exutil.NewCLI(openstackNamespaceName)

	g.BeforeEach(func(ctx g.SpecContext) {
		g.By(fmt.Sprintf("Checking the %s, %s and %s env vars are being set", shiftstackKubeConfigEnvVarName, shiftstackPassFileEnvVarName, rhosoKubeConfigEnvVarName))
		if shiftstackKubeConfig == "" || shiftstackPassFile == "" || rhosoKubeConfig == "" {
			e2eskipper.Skipf("%s, %s and %s env vars must be set for this test to run", shiftstackKubeConfigEnvVarName, shiftstackPassFileEnvVarName, rhosoKubeConfigEnvVarName)
		}

		g.By("Getting the Openstack clients")
		computeClient, err = client.GetServiceClient(ctx, openstack.NewComputeV2)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get the OpenStack compute client")
		volumeClient, err = client.GetServiceClient(ctx, openstack.NewBlockStorageV3)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get the OpenStack block storage client")
	})

	g.It("should trigger prometheus to add the rhoso target", func(ctx g.SpecContext) {
		shiftstackPrometheusFederateRouteName := "prometheus-k8s-federate"
		monitoringNamespaceName := "openshift-monitoring"
		metricStoragePrometheusRouteName := "metric-storage-prometheus"
		secretName := "ocp-federated"
		scrapeConfigName := "sos-federated"
		kubeNodeInfoQuery := "group by (node, provider_id) (kube_node_info)"
		kubePersistentVolumeInfoQuery := "group by (csi_volume_handle, persistentvolume) (kube_persistentvolume_info)"

		// Get the shiftstack prometheus federate endpoint (in order to scrape metrics from it)
		g.By(fmt.Sprintf("Getting the '%s' route host from the shiftstack cluster", shiftstackPrometheusFederateRouteName))
		shiftstackConfig, err := clientcmd.BuildConfigFromFlags("", shiftstackKubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())
		shiftstackRouteClient, err := routev1.NewForConfig(shiftstackConfig)
		o.Expect(err).NotTo(o.HaveOccurred())
		shiftstackFederateRoute, err := shiftstackRouteClient.Routes(monitoringNamespaceName).Get(ctx, shiftstackPrometheusFederateRouteName, metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		shiftstackFederateRouteHost := shiftstackFederateRoute.Status.Ingress[0].Host
		o.Expect(shiftstackFederateRouteHost).NotTo(o.BeEmpty(), "Empty '%s' route host found", shiftstackPrometheusFederateRouteName)
		e2e.Logf("Shiftstack federate route host: '%v'", shiftstackFederateRouteHost)

		// Create a token in the shiftstack cluster
		g.By("Creating a token in the shiftstack cluster")
		SetTestContextHostFromKubeconfig(shiftstackKubeConfig)
		pass, err := ioutil.ReadFile(shiftstackPassFile)
		o.Expect(err).NotTo(o.HaveOccurred())
		_, err = oc.Run("login").Args("-u", "kubeadmin").InputString(string(pass) + "\n").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		shiftstackToken, err := oc.Run("whoami").Args("-t").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("Token: '%v'", shiftstackToken)

		// Create a secret in Openstack with the shiftstack token
		g.By(fmt.Sprintf("Creating the '%s' secret in Openstack with the shiftstack token", secretName))
		SetTestContextHostFromKubeconfig(rhosoKubeConfig)

		secretDef := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: openstackNamespaceName,
			},
			Data: map[string][]byte{
				"token": []byte(shiftstackToken),
			},
		}

		// Load Kubernetes client for RHOSO OCP cluster
		rhosoClientSet, err := e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())
		_, err = rhosoClientSet.ServerVersion()
		o.Expect(err).NotTo(o.HaveOccurred(), fmt.Sprintf("Kubernetes API Access Error: %v", err))
		client := rhosoClientSet.CoreV1().Secrets(openstackNamespaceName)
		_, err = client.Create(ctx, secretDef, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred(), fmt.Sprintf("Error creating the '%s' secret in Openstack: %v", secretName, err))
		defer client.Delete(ctx, secretName, metav1.DeleteOptions{})

		// Get the Openstack Prometheus metric storage route
		g.By(fmt.Sprintf("Getting the '%s' route host from Openstack", metricStoragePrometheusRouteName))
		rhosoConfig, err := clientcmd.BuildConfigFromFlags("", rhosoKubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())
		rhosoRouteClient, err := routev1.NewForConfig(rhosoConfig)
		o.Expect(err).NotTo(o.HaveOccurred())
		metricStorageRoute, err := rhosoRouteClient.Routes(openstackNamespaceName).Get(ctx, metricStoragePrometheusRouteName, metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		metricStorageRouteHost := metricStorageRoute.Status.Ingress[0].Host
		o.Expect(metricStorageRouteHost).NotTo(o.BeEmpty(), "Empty '%s' route host found", metricStoragePrometheusRouteName)
		e2e.Logf("Openstack metric storage route host: '%v'", metricStorageRouteHost)

		// Wait until the monitoring target is down (could be still up if same test ran before)
		metricStorageURL := fmt.Sprintf("https://%s/api/v1/query", metricStorageRouteHost)
		prometheusTarget := fmt.Sprintf("scrapeConfig/openstack/%s", scrapeConfigName)
		g.By(fmt.Sprintf("Waiting until '%s' monitoring target is down (from any previous test) in Openstack Prometheus", prometheusTarget))
		o.Expect(waitUntilTargetStatus(metricStorageURL, prometheusTarget, "down")).NotTo(o.HaveOccurred(), "Error waiting for monitoring target status")

		// Create a scrapeconfig in Openstack (will scrape metrics from shiftstack cluster)
		g.By(fmt.Sprintf("Creating the '%s' scrapeconfig in Openstack", scrapeConfigName))
		gvr := schema.GroupVersionResource{
			Group:    "monitoring.rhobs",
			Version:  "v1alpha1",
			Resource: "scrapeconfigs",
		}

		scrapeConfig := map[string]interface{}{
			"apiVersion": "monitoring.rhobs/v1alpha1",
			"kind":       "ScrapeConfig",
			"metadata": map[string]interface{}{
				"name":      scrapeConfigName,
				"namespace": openstackNamespaceName,
				"labels": map[string]interface{}{
					"service": "metricStorage",
				},
			},
			"spec": map[string]interface{}{
				"scheme":         "HTTPS",
				"metricsPath":    "federate",
				"scrapeInterval": scrapeInterval,
				"params": map[string]interface{}{
					"match[]": []string{
						`{__name__=~"kube_node_info|kube_persistentvolume_info"}`,
					},
				},
				"authorization": map[string]interface{}{
					"type": "Bearer",
					"credentials": map[string]interface{}{
						"name": secretName,
						"key":  "token",
					},
				},
				"tlsConfig": map[string]interface{}{
					"insecureSkipVerify": true,
				},
				"staticConfigs": []interface{}{
					map[string]interface{}{
						"targets": []string{
							shiftstackFederateRouteHost,
						},
					},
				},
			},
		}

		rhosoCfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		dcRhoso, err := dynamic.NewForConfig(rhosoCfg)
		o.Expect(err).NotTo(o.HaveOccurred())
		resourceClient := dcRhoso.Resource(gvr).Namespace(openstackNamespaceName)
		_, err = resourceClient.Create(ctx, &unstructured.Unstructured{
			Object: scrapeConfig,
		}, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		defer resourceClient.Delete(ctx, scrapeConfigName, metav1.DeleteOptions{})

		// Wait until the monitoring target is up
		g.By(fmt.Sprintf("Waiting until '%s' monitoring target is up in Openstack Prometheus", prometheusTarget))
		o.Expect(waitUntilTargetStatus(metricStorageURL, prometheusTarget, "up")).NotTo(o.HaveOccurred(), "Error waiting for monitoring target status")

		// Get Openstack servers (needed to contrast the kube_node_info metrics content) and store the names and ids in a map
		g.By("Getting the Openstack servers")
		serversAllPages, err := servers.List(computeClient, nil).AllPages(ctx)
		o.Expect(err).NotTo(o.HaveOccurred())
		allServers, err := servers.ExtractServers(serversAllPages)
		o.Expect(err).NotTo(o.HaveOccurred())
		openstackServerMap := make(map[string]string)
		for _, server := range allServers {
			e2e.Logf("Server found (ID: %v, Name: %v, Status: %v)", server.ID, server.Name, server.Status)
			openstackServerMap[server.Name] = server.ID
		}
		e2e.Logf("Openstack servers map: '%v'", openstackServerMap)

		// Set context host for shiftstack cluster
		SetTestContextHostFromKubeconfig(shiftstackKubeConfig)

		// Load Kubernetes client for shiftstack cluster
		shiftstackClientSet, err := e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())
		_, err = shiftstackClientSet.ServerVersion()
		o.Expect(err).NotTo(o.HaveOccurred(), fmt.Sprintf("Kubernetes API Access Error: '%v'", err))

		// Sleep scrapeInterval + additional 10s before retrieving the kube_node_info metrics to allow time to scrape them
		g.By(fmt.Sprintf("Sleeping the scrapeInterval '%s' + additional 10s before retrieving the kube_node_info metrics", scrapeInterval))
		scrapeIntervalDuration, err := time.ParseDuration(scrapeInterval)
		o.Expect(err).NotTo(o.HaveOccurred(), fmt.Sprintf("Error parsing scrapeInterval duration '%v'", scrapeInterval))
		time.Sleep(scrapeIntervalDuration + 10*time.Second)

		// Get kube_node_info metrics
		nodeMetricsMap := getKubeNodeInfoMetricInMap(metricStorageURL, kubeNodeInfoQuery)
		e2e.Logf("Nodes from metrics: '%v'", nodeMetricsMap)

		// Get shiftstack cluster nodes
		nodeList, err := shiftstackClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		// Check the shiftstack cluster node list and node map length are the same
		g.By("Checking the shiftstack cluster node list and node map length are the same")
		o.Expect(len(nodeList.Items)).To(o.Equal(len(nodeMetricsMap)),
			"Shiftstack cluster node list '%v' and node map '%v' should have the same length", nodeList.Items, nodeMetricsMap)
		e2e.Logf("Length of the shiftstack cluster node list and of the node map obtained from the metrics is the same ('%d')", len(nodeList.Items))

		// Check each shiftstack cluster node is in the node map obtained from the metrics
		g.By("Checking each shiftstack cluster node is in the node map obtained from the metrics")
		for _, node := range nodeList.Items {
			o.Expect(nodeMetricsMap).To(o.HaveKey(node.Name),
				"Node '%s' should be in the node map obtained from the metrics '%v'", node.Name, nodeMetricsMap)
			e2e.Logf("Node '%s' found in the node map obtained from the metrics", node.Name)
		}

		// Check every node obtained from metrics is in the servers info from Openstack
		g.By("Checking every node obtained from metrics is in the servers info from Openstack")
		for serverName, serverID := range nodeMetricsMap {
			o.Expect(openstackServerMap).To(o.HaveKeyWithValue(serverName, serverID),
				"Entry '%s: %s' should be present in the servers map obtained from Openstack '%v'", serverName, serverID, openstackServerMap)
			e2e.Logf("'%s: %s' entry found in the servers map obtained from Openstack", serverName, serverID)
		}

		// Create a cinder PVC in shiftstack cluster
		ns := oc.Namespace()
		g.By(fmt.Sprintf("Creating a PVC for a cinder volume in '%v' namespace in the shiftstack cluster", ns))
		cinderSc := FindStorageClassByProvider(oc, "cinder.csi.openstack.org", true)
		o.Expect(cinderSc).NotTo(o.BeNil(), "default cinder-csi storageClass not found.")
		pvc := CreatePVC(ctx, shiftstackClientSet, "cinder-pvc", ns, cinderSc.Name, "1Gi")

		// Create a deployment with the volume attached
		g.By(fmt.Sprintf("Creating Openshift deployment with 1 replica and cinder volume '%v' attached", pvc.Name))
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

		deployment, err := shiftstackClientSet.AppsV1().Deployments(ns).Create(ctx, testDeployment, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		err = e2edeployment.WaitForDeploymentComplete(shiftstackClientSet, deployment)
		o.Expect(err).NotTo(o.HaveOccurred())

		// Get volume info in Openstack
		g.By("Getting the Openstack volume")
		pvcVolumeName, err := waitPvcVolume(ctx, shiftstackClientSet, pvc.Name, ns)
		o.Expect(err).NotTo(o.HaveOccurred())
		cinderVolumes, err := getVolumesFromName(ctx, volumeClient, pvcVolumeName)
		o.Expect(err).NotTo(o.HaveOccurred(), "Error gathering Openstack volume info for PVC '%s'", pvc.Name)
		o.Expect(cinderVolumes).To(o.HaveLen(1), "Unexpected number of volumes for PVC '%s'", pvc.Name)
		volumeID := cinderVolumes[0].ID
		e2e.Logf("Volume ID '%v' for volume name '%v' found in Openstack for PVC '%s'", volumeID, pvcVolumeName, pvc.Name)

		// Get Openstack volumes (needed to contrast the kube_persistentvolume_info metrics content) and store the names and ids in a map
		g.By("Getting the Openstack volumes")
		volumesAllPages, err := volumes.List(volumeClient, nil).AllPages(ctx)
		o.Expect(err).NotTo(o.HaveOccurred())
		allVolumes, err := volumes.ExtractVolumes(volumesAllPages)
		o.Expect(err).NotTo(o.HaveOccurred())
		openstackVolumeMap := make(map[string]string)
		for _, volume := range allVolumes {
			e2e.Logf("Volume found (ID: %v, Name: %v, Status: %v)", volume.ID, volume.Name, volume.Status)
			openstackVolumeMap[volume.Name] = volume.ID
		}
		e2e.Logf("Openstack volume map: '%v'", openstackVolumeMap)

		// Sleep scrapeInterval + additional 10s before retrieving the kube_persistentvolume_info metrics to allow time to scrape them
		g.By(fmt.Sprintf("Sleeping the scrapeInterval '%s' + additional 10s before retrieving the kube_persistentvolume_info metrics", scrapeInterval))
		time.Sleep(scrapeIntervalDuration + 10*time.Second)

		// Get kube_persistentvolume_info metrics
		volumeMetricsMap := getKubePersistentvolumeInfoMetricInMap(metricStorageURL, kubePersistentVolumeInfoQuery)
		e2e.Logf("Volumes from metrics: '%v'", volumeMetricsMap)

		// Get shiftstack cluster pvcs from all namespaces
		pvcList, err := shiftstackClientSet.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		// Check the shiftstack pvc list and the volume map length are the same
		g.By("Checking the shiftstack pvc list and volume map length are the same")
		o.Expect(len(pvcList.Items)).To(o.Equal(len(volumeMetricsMap)),
			"Shiftstack pvc list '%v' and volume map '%v' should have the same length", pvcList.Items, volumeMetricsMap)
		e2e.Logf("Length of the shiftstack pvc list and of the volume map obtained from the metrics is the same ('%d')", len(pvcList.Items))

		// Check each shiftstack pvc is in the volume map obtained from the metrics
		g.By("Checking each shiftstack pvc is in the volume map obtained from the metrics")
		for _, pvc := range pvcList.Items {
			o.Expect(volumeMetricsMap).To(o.HaveKey(pvc.Spec.VolumeName),
				"PVC '%s' should be in the volume map obtained from the metrics '%v'", pvc.Spec.VolumeName, volumeMetricsMap)
			e2e.Logf("PVC '%s' found in the volume map obtained from the metrics", pvc.Spec.VolumeName)
		}

		// Check every volume obtained from metrics is in the volumes info from Openstack
		g.By("Checking every volume obtained from metrics is in the volumes info from Openstack")
		for volumeName, volumeID := range volumeMetricsMap {
			o.Expect(openstackVolumeMap).To(o.HaveKeyWithValue(volumeName, volumeID),
				"Entry '%s: %s' should be present in the volume map obtained from Openstack '%v'", volumeName, volumeID, openstackVolumeMap)
			e2e.Logf("'%s: %s' entry found in the volume map obtained from Openstack", volumeName, volumeID)
		}
	})
})

// Set the TestContextHost based on the Kubeconfig
func SetTestContextHostFromKubeconfig(kubeConfigPath string) error {

	e2e.TestContext.KubeConfig = kubeConfigPath

	e2e.Logf("Loading kubeconfig from '%v'", kubeConfigPath)
	kubeConfig, err := clientcmd.LoadFromFile(kubeConfigPath)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	currentContext := kubeConfig.CurrentContext
	context, ok := kubeConfig.Contexts[currentContext]
	if !ok {
		return fmt.Errorf("context %q not found in kubeconfig", currentContext)
	}

	cluster, ok := kubeConfig.Clusters[context.Cluster]
	if !ok {
		return fmt.Errorf("cluster %q not found in kubeconfig", context.Cluster)
	}

	e2e.TestContext.Host = cluster.Server
	e2e.Logf("TestContext Host: '%v'", e2e.TestContext.Host)

	return nil
}

func makeGETRequest(baseURL string, promQLQuery string) (string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	defer client.CloseIdleConnections()

	params := url.Values{}
	params.Set("query", promQLQuery)
	fullURL := baseURL + "?" + params.Encode()
	e2e.Logf("Request: '%s'", fullURL)
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		fmt.Println("Error creating request:", err)
		return "", err
	}

	req.Header.Add("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Error sending request:", err)
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	e2e.Logf("Response: '%v' - '%v'", resp.Status, string(body))

	return string(body), nil
}

// Get kube_node_info metrics node and provider_id fields in a map
func getKubeNodeInfoMetricInMap(baseURL string, query string) map[string]string {
	e2e.Logf("Getting metrics (query: '%v') from '%v'", query, baseURL)

	body, err := makeGETRequest(baseURL, query)
	if err != nil {
		e2e.Logf("GET request failed: '%v'", err)
		return nil
	}

	type Metric struct {
		Node       string `json:"node"`
		ProviderID string `json:"provider_id"`
	}

	type Result struct {
		Metric Metric         `json:"metric"`
		Value  [2]interface{} `json:"value"`
	}

	type Data struct {
		ResultType string   `json:"resultType"`
		Result     []Result `json:"result"`
	}

	type Response struct {
		Status string `json:"status"`
		Data   Data   `json:"data"`
	}

	var r Response
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		fmt.Println(err)
	}
	e2e.Logf("Metrics query response: '%v'", r)

	nodes := make(map[string]string)
	for _, f := range r.Data.Result {
		instanceID := strings.TrimPrefix(f.Metric.ProviderID, "openstack:///")
		nodes[f.Metric.Node] = instanceID
	}

	return nodes
}

// Get kube_persistentvolume_info metrics csi_volume_handle and persistentvolume fields in a map
func getKubePersistentvolumeInfoMetricInMap(baseURL string, query string) map[string]string {
	e2e.Logf("Getting metrics (query: '%v') from '%v'", query, baseURL)

	body, err := makeGETRequest(baseURL, query)
	if err != nil {
		e2e.Logf("GET request failed: '%v'", err)
		return nil
	}

	type Metric struct {
		CSIVolumeHandle string `json:"csi_volume_handle"`
		PersistenVolume string `json:"persistentvolume"`
	}

	type Result struct {
		Metric Metric         `json:"metric"`
		Value  [2]interface{} `json:"value"`
	}

	type Data struct {
		ResultType string   `json:"resultType"`
		Result     []Result `json:"result"`
	}

	type Response struct {
		Status string `json:"status"`
		Data   Data   `json:"data"`
	}

	var r Response
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		fmt.Println(err)
	}
	e2e.Logf("Metrics query response: '%v'", r)

	volumes := make(map[string]string)
	for _, f := range r.Data.Result {
		volumes[f.Metric.PersistenVolume] = f.Metric.CSIVolumeHandle
	}

	return volumes
}

// Active wait until a given monitoring target is in desired status (can be "up or "down")
func waitUntilTargetStatus(baseURL string, target string, targetStatus string) error {
	var err error

	expectedUpMetricValue := "0"
	if targetStatus == "up" {
		expectedUpMetricValue = "1"
	}

	upQuery := fmt.Sprintf("up{job=\"%s\"}", target)
	e2e.Logf("baseURL: '%v'", baseURL)
	e2e.Logf("PromQL query: '%v'", upQuery)

	type Response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []interface{}     `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}

	o.Eventually(func() string {
		var resp Response
		body, err := makeGETRequest(baseURL, upQuery)
		if err != nil {
			e2e.Logf("GET request failed: %v, trying next iteration\n", err)
			return ""
		}

		err = json.Unmarshal([]byte(body), &resp)
		if err != nil {
			e2e.Logf("Error parsing JSON: %v, trying next iteration\n", err)
			return ""
		}

		// Extract the Up metric value (second element in the "value" array)
		if len(resp.Data.Result) > 0 && len(resp.Data.Result[0].Value) > 1 {
			metricValue, ok := resp.Data.Result[0].Value[1].(string) // Ensure it's a string
			if ok {
				e2e.Logf("Up metric value: '%v'", metricValue)
				return metricValue
			} else {
				e2e.Logf("Unexpected Up metric value type: '%v'", metricValue)
			}
		} else {
			e2e.Logf("Up metric value is missing or empty")
			return "0"
		}

		return ""
	}, "480s", "30s").Should(o.Equal(expectedUpMetricValue), "Timed out waiting the Up metric to be '%v'", expectedUpMetricValue)

	return err
}
