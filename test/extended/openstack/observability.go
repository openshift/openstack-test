package openstack

import (
	"fmt"
	"io/ioutil"
	"os"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

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
	rhosoKubeConfig := os.Getenv("RHOSO_KUBECONFIG")
	oc := exutil.NewCLI("openstack")

	g.BeforeEach(func(ctx g.SpecContext) {
		if rhosoKubeConfig == "" {
			e2eskipper.Skipf("RHOSO_KUBECONFIG must be set for this test to run")
		}
	})

	g.It("should trigger prometheus to add the rhoso target", func(ctx g.SpecContext) {
		scrapeCfgName := "sos-federated"

		g.By("Get route from shiftstack cluster")
		shiftstacKubeConfig := os.Getenv("KUBECONFIG")
		e2e.TestContext.KubeConfig = shiftstacKubeConfig
		SetTestContextHostFromKubeconfig(shiftstacKubeConfig)
		route, err := oc.AdminRouteClient().RouteV1().Routes("openshift-monitoring").Get(ctx, "prometheus-k8s-federate", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		routeHost := route.Status.Ingress[0].Host
		e2e.Logf("Route Host: %v", route.Status.Ingress[0].Host)

		pass, err := ioutil.ReadFile(os.Getenv("SHIFTSTACK_PASS_FILE"))
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
		g.By("Creating ScrapeConfig....")
		resourceClient := dcRhoso.Resource(gvr).Namespace("openstack")
		_, err = resourceClient.Create(ctx, &unstructured.Unstructured{
			Object: scrapeConfig,
		}, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		defer resourceClient.Delete(ctx, scrapeCfgName, metav1.DeleteOptions{})
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
