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
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	"github.com/openshift/openstack-test/test/extended/openstack/client"
	exutil "github.com/openshift/origin/test/extended/util"

	v1core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] Creating ScrapeConfig in rhoso", func() {
	defer g.GinkgoRecover()

	oc := exutil.NewCLI("openstack")
	shiftstackKubeConfigEnvVarName := "KUBECONFIG"
	shiftstackPassFileEnvVarName := "SHIFTSTACK_PASS_FILE"
	rhosoKubeConfigEnvVarName := "RHOSO_KUBECONFIG"
	shiftstackKubeConfig := os.Getenv(shiftstackKubeConfigEnvVarName)
	rhosoKubeConfig := os.Getenv(rhosoKubeConfigEnvVarName)
	shiftstackPassFile := os.Getenv(shiftstackPassFileEnvVarName)
	var computeClient *gophercloud.ServiceClient
	var err error

	g.BeforeEach(func(ctx g.SpecContext) {
		g.By(fmt.Sprintf("Checking the %s, %s and %s env vars are being set", shiftstackKubeConfigEnvVarName, shiftstackPassFileEnvVarName, rhosoKubeConfigEnvVarName))
		if shiftstackKubeConfig == "" || shiftstackPassFile == "" || rhosoKubeConfig == "" {
			e2eskipper.Skipf("%s, %s and %s env vars must be set for this test to run", shiftstackKubeConfigEnvVarName, shiftstackPassFileEnvVarName, rhosoKubeConfigEnvVarName)
		}

		g.By("Getting the Openstack compute client")
		computeClient, err = client.GetServiceClient(ctx, openstack.NewComputeV2)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get the OpenStack compute client")
	})

	g.It("should trigger prometheus to add the rhoso target", func(ctx g.SpecContext) {
		shiftstackPrometheusFederateRouteName := "prometheus-k8s-federate"
		monitoringNamespaceName := "openshift-monitoring"
		scrapeCfgName := "sos-federated"

		// Get the shiftstack prometheus federate endpoint (in order to scrape metrics from it)
		g.By(fmt.Sprintf("Getting the '%s' route host from shiftstack cluster", shiftstackPrometheusFederateRouteName))
		e2e.TestContext.KubeConfig = shiftstackKubeConfig
		SetTestContextHostFromKubeconfig(shiftstackKubeConfig)
		route, err := oc.AdminRouteClient().RouteV1().Routes(monitoringNamespaceName).Get(ctx, shiftstackPrometheusFederateRouteName, metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		routeHost := route.Status.Ingress[0].Host
		o.Expect(routeHost).NotTo(o.BeEmpty(), "Empty %s route host found", shiftstackPrometheusFederateRouteName)
		e2e.Logf("Route Host: %v", routeHost)

		pass, err := ioutil.ReadFile(shiftstackPassFile)
		o.Expect(err).NotTo(o.HaveOccurred())
		_, err = oc.Run("login").Args("-u", "kubeadmin").InputString(string(pass) + "\n").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		token, err := oc.Run("whoami").Args("-t").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("Token:%v", token)

		g.By("Creating a secret with token on rhoso openstack namespace")
		e2e.TestContext.KubeConfig = rhosoKubeConfig
		SetTestContextHostFromKubeconfig(rhosoKubeConfig)
		rhosoCfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		clientSet, err := e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		secretData := map[string][]byte{
			"token": []byte(token),
		}
		secretDef := &v1core.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ocp-federated",
			},
			Data: secretData,
		}
		client := clientSet.CoreV1().Secrets("openstack")
		_, err = client.Create(ctx, secretDef, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		defer client.Delete(ctx, "ocp-federated", metav1.DeleteOptions{})

		dcRhoso, err := dynamic.NewForConfig(rhosoCfg)
		o.Expect(err).NotTo(o.HaveOccurred())

		gvr := schema.GroupVersionResource{
			Group:    "monitoring.rhobs",
			Version:  "v1alpha1",
			Resource: "scrapeconfigs",
		}

		scrapeConfig := map[string]interface{}{
			"apiVersion": "monitoring.rhobs/v1alpha1",
			"kind":       "ScrapeConfig",
			"metadata": map[string]interface{}{
				"name":      scrapeCfgName,
				"namespace": "openstack",
				"labels": map[string]interface{}{
					"service": "metricStorage",
				},
			},
			"spec": map[string]interface{}{
				"scheme":         "HTTPS",
				"metricsPath":    "federate",
				"scrapeInterval": "30s",
				"params": map[string]interface{}{
					"match[]": []string{
						`{__name__=~"kube_node_info|kube_persistentvolume_info"}`,
					},
				},
				"authorization": map[string]interface{}{
					"type": "Bearer",
					"credentials": map[string]interface{}{
						"name": "ocp-federated",
						"key":  "token",
					},
				},
				"tlsConfig": map[string]interface{}{
					"insecureSkipVerify": true,
				},
				"staticConfigs": []interface{}{
					map[string]interface{}{
						"targets": []string{
							routeHost,
						},
					},
				},
			},
		}
		g.By("Creating ScrapeConfig")
		resourceClient := dcRhoso.Resource(gvr).Namespace("openstack")
		_, err = resourceClient.Create(ctx, &unstructured.Unstructured{
			Object: scrapeConfig,
		}, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		defer resourceClient.Delete(ctx, scrapeCfgName, metav1.DeleteOptions{})

		// Get Openstack servers (needed to contrast the kube_node_info metric content) and store the names and ids in a map
		g.By("Getting the Openstack servers")
		allPages, err := servers.List(computeClient, nil).AllPages(ctx)
		o.Expect(err).NotTo(o.HaveOccurred())
		allServers, err := servers.ExtractServers(allPages)
		o.Expect(err).NotTo(o.HaveOccurred())
		openstackServerMap := make(map[string]string)
		for _, server := range allServers {
			//OS_CLOUD=default is needed in order to get servers' Host (TODO: remove this comment)
			e2e.Logf("Server found (ID: %v, Name: %v, Status: %v)", server.ID, server.Name, server.Status)
			openstackServerMap[server.Name] = server.ID
		}
		e2e.Logf("Openstack servers map: %v)", openstackServerMap)
		//time.Sleep(time.Minute * 3)
		e2e.Logf("Nodes from metrics: %v", getMetrics())
		time.Sleep(time.Minute * 60)

	})
})

// Set the TestContextHost based on the Kubeconfig
func SetTestContextHostFromKubeconfig(kubeConfigPath string) error {
	e2e.TestContext.KubeConfig = kubeConfigPath

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
	return nil
}

func makeGETRequest(baseURL string) (string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	defer client.CloseIdleConnections()

	params := url.Values{}
	params.Set("query", "group by (node, provider_id) (kube_node_info)")
	fullURL := baseURL + "?" + params.Encode()
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

	return string(body), nil
}

func getMetrics() map[string]string {
	// GET request example
	getURL := "https://metric-storage-prometheus-openstack.apps.ocp.openstack.lab/api/v1/query"

	body, err := makeGETRequest(getURL)
	if err != nil {
		fmt.Printf("GET request failed: %v\n", err)
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
	//fmt.Println(r)

	nodes := make(map[string]string)

	for _, f := range r.Data.Result {
		instanceID := strings.TrimLeft(f.Metric.ProviderID, "openstack:///")
		nodes[f.Metric.Node] = instanceID

	}
	//fmt.Println(nodes)
	return nodes
}
