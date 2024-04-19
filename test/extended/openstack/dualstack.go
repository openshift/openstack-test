package openstack

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	octavialoadbalancers "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	ini "gopkg.in/ini.v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2edeployment "k8s.io/kubernetes/test/e2e/framework/deployment"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform", func() {
	oc := exutil.NewCLI("openstack")
	var loadBalancerClient *gophercloud.ServiceClient
	var networkClient *gophercloud.ServiceClient
	var clientSet *kubernetes.Clientset
	var cloudProviderConfig *ini.File
	var loadBalancerServiceTimeout = 10 * time.Minute
	availableLbProvidersUnderTests := [...]string{"Amphora", "OVN"}
	ipFamilyPolicy := "PreferDualStack"
	ipFamilies := [][]v1.IPFamily{{v1.IPv4Protocol, v1.IPv6Protocol}, {v1.IPv6Protocol, v1.IPv4Protocol}}

	g.BeforeEach(func(ctx g.SpecContext) {
		network, err := oc.AdminConfigClient().ConfigV1().Networks().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		dualstack, err := isDualStackCluster(network.Status.ClusterNetwork)
		o.Expect(err).NotTo(o.HaveOccurred())

		if !dualstack {
			e2eskipper.Skipf("Tests are applicable for dualstack only")
		}

		// TODO revert once https://issues.redhat.com/browse/OSASINFRA-3079 is resolved
		// For now, LoadBalancer tests are not applicable when the cluster is running behind a proxy.
		proxy, err := oc.AdminConfigClient().ConfigV1().Proxies().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		if os.Getenv("HTTP_PROXY") != "" || os.Getenv("HTTPS_PROXY") != "" || proxy.Status.HTTPProxy != "" {
			e2eskipper.Skipf("Test not applicable for proxy setup")
		}

		g.By("preparing openstack client")
		loadBalancerClient, err = client(serviceLoadBalancer)
		o.Expect(err).NotTo(o.HaveOccurred(), "Error creating an openstack LoadBalancer client")
		networkClient, err = client(serviceNetwork)
		o.Expect(err).NotTo(o.HaveOccurred(), "Error creating an openstack network client")

		g.By("preparing openshift dynamic client")
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Gathering cloud-provider-config")
		cloudProviderConfig, err = getConfig(ctx,
			oc.AdminKubeClient(),
			"openshift-cloud-controller-manager",
			"cloud-conf",
			"cloud.conf")
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	for _, i := range availableLbProvidersUnderTests {
		lbProviderUnderTest := i
		protocolUnderTest := v1.Protocol("TCP")
		for _, ipFamiliesList := range ipFamilies {
			g.It(fmt.Sprintf("should create a %s %s LoadBalancer when a %s svc with type:LoadBalancer and IP family policy %s and IP families %s is created on Openshift", protocolUnderTest, lbProviderUnderTest, protocolUnderTest, ipFamilyPolicy, ipFamiliesList), func(ctx g.SpecContext) {
				fmt.Fprintf(g.GinkgoWriter, "Some log text: %v\n", lbProviderUnderTest)
				skipIfNotLbProvider(lbProviderUnderTest, cloudProviderConfig)

				g.By("Creating Openshift deployment")
				labels := map[string]string{"app": "lb-dualstack-dep"}
				testDeployment := createTestDeployment(deploymentOpts{
					Name:     "lb-dualstack-dep",
					Labels:   labels,
					Replicas: 2,
					Protocol: protocolUnderTest,
					Port:     8081,
				})
				deployment, err := clientSet.AppsV1().Deployments(oc.Namespace()).Create(ctx,
					testDeployment, metav1.CreateOptions{})
				o.Expect(err).NotTo(o.HaveOccurred())
				err = e2edeployment.WaitForDeploymentComplete(clientSet, deployment)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Creating Openshift LoadBalancer Service")
				svcName := "lb-default-svc"
				svcPort := int32(8082)
				jig := e2eservice.NewTestJig(clientSet, oc.Namespace(), svcName)
				jig.Labels = labels
				svc, err := jig.CreateLoadBalancerService(ctx, loadBalancerServiceTimeout, func(svc *v1.Service) {
					svc.Spec.Ports = []v1.ServicePort{{Protocol: protocolUnderTest, Port: svcPort, TargetPort: intstr.FromInt(8081)}}
					svc.Spec.Selector = labels
					svc.Spec.IPFamilyPolicy = (*v1.IPFamilyPolicy)(&ipFamilyPolicy)
					svc.Spec.IPFamilies = []v1.IPFamily{v1.IPFamily(ipFamiliesList[0]), v1.IPFamily(ipFamiliesList[1])}
				})
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Checks from openshift perspective")
				loadBalancerId := svc.GetAnnotations()["loadbalancer.openstack.org/load-balancer-id"]
				o.Expect(loadBalancerId).ShouldNot(o.BeEmpty(), "load-balancer-id annotation missing")
				o.Expect(svc.Status.LoadBalancer.Ingress).ShouldNot(o.BeEmpty(), "svc.Status.LoadBalancer.Ingress should not be empty")
				svcIp := svc.Status.LoadBalancer.Ingress[0].IP
				o.Expect(svcIp).ShouldNot(o.BeEmpty(), "Ingress IP missing on svc Status")
				o.Expect(svc.Spec.ExternalTrafficPolicy).Should(o.Equal(v1.ServiceExternalTrafficPolicyTypeCluster),
					"Unexpected ExternalTrafficPolicy on svc specs")

				g.By("Checks from openstack perspective")
				lb, err := octavialoadbalancers.Get(loadBalancerClient, loadBalancerId).Extract()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(lb.Provider).Should(o.Equal(strings.ToLower(lbProviderUnderTest)), "Unexpected provider in the Openstack LoadBalancer")
				if isIpv4(lb.VipAddress) { // No FIP assignment on ipv6
					fip, err := getFipbyFixedIP(networkClient, lb.VipAddress)
					o.Expect(err).NotTo(o.HaveOccurred())
					o.Expect(fip.FloatingIP).Should(o.Equal(svcIp), "Unexpected floatingIp in the Openstack LoadBalancer")
				}

				g.By("accessing the service 100 times from outside and storing the name of the pods answering")
				results := make(map[string]int)
				for i := 0; i < 100; i++ {
					// https://github.com/kubernetes/kubernetes/blob/master/test/images/agnhost/README.md#netexec
					_, err := getPodNameThroughLb(svcIp, fmt.Sprintf("%d", svcPort), protocolUnderTest, "hostname")
					if err != nil {
						e2e.Logf("Error detected while accessing the LoadBalancer service on try %d: %q", i, err)
					}
				}
				e2e.Logf("Pods accessed after 100 requests:\n%v\n", results)
			})
		}
	}
})
