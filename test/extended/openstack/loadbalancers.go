package openstack

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	octavialoadbalancers "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	ini "gopkg.in/ini.v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2edeployment "k8s.io/kubernetes/test/e2e/framework/deployment"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	imageutils "k8s.io/kubernetes/test/utils/image"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack][lb] The Openstack platform", func() {

	oc := exutil.NewCLI("openstack")
	var loadBalancerClient *gophercloud.ServiceClient
	var networkClient *gophercloud.ServiceClient
	var clientSet *kubernetes.Clientset

	g.BeforeEach(func() {

		network, err := oc.AdminConfigClient().ConfigV1().Networks().Get(context.Background(), "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		if network.Status.NetworkType == "Kuryr" {
			e2eskipper.Skipf("Test not applicable for Kuryr NetworkType")
		}

		g.By("preparing openstack client")
		loadBalancerClient, err = client(serviceLoadBalancer)
		o.Expect(err).NotTo(o.HaveOccurred(), "Error creating an openstack LoadBalancer client")
		networkClient, err = client(serviceNetwork)
		o.Expect(err).NotTo(o.HaveOccurred(), "Error creating an openstack network client")

		g.By("preparing openshift dynamic client")
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	// https://issues.redhat.com/browse/OSASINFRA-2753
	g.It("should create an UDP Amphora LoadBalancer when an UDP svc with type:LoadBalancer is created on Openshift", func() {

		g.By("Checking loadBalancer provider")
		cloudProviderConfig, err := getConfig(oc.AdminKubeClient(),
			"openshift-config",
			"cloud-provider-config",
			"config")
		o.Expect(err).NotTo(o.HaveOccurred())
		lbProvider, err := getClusterLoadBalancerSetting("lb-provider", cloudProviderConfig)
		o.Expect(err).NotTo(o.HaveOccurred())
		if lbProvider != "amphora" {
			e2eskipper.Skipf("Test not applicable for LoadBalancer provider different than Amphora: %q", lbProvider)
		}

		g.By("Creating Openshift resources")
		labels := map[string]string{"app": "udp-lb-default-dep"}
		depName := "udp-lb-default-dep"
		svcName := "udp-lb-default-svc"
		testDeployment := e2edeployment.NewDeployment(depName, 2, labels, "udp-test",
			imageutils.GetE2EImage(imageutils.Agnhost), appsv1.RollingUpdateDeploymentStrategyType)
		testDeployment.Spec.Template.Spec.SecurityContext = e2epod.GetRestrictedPodSecurityContext()
		testDeployment.Spec.Template.Spec.Containers[0].SecurityContext = e2epod.GetRestrictedContainerSecurityContext()
		testDeployment.Spec.Template.Spec.Containers[0].Args = []string{"netexec", "--udp-port=8081"}
		testDeployment.Spec.Template.Spec.Containers[0].Ports = []v1.ContainerPort{{
			ContainerPort: 8081,
			Protocol:      "UDP",
		}}
		deployment, err := clientSet.AppsV1().Deployments(oc.Namespace()).Create(context.TODO(),
			testDeployment, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		err = e2edeployment.WaitForDeploymentComplete(clientSet, deployment)
		o.Expect(err).NotTo(o.HaveOccurred())

		jig := e2eservice.NewTestJig(clientSet, oc.Namespace(), svcName)
		jig.Labels = labels
		svc, err := jig.CreateLoadBalancerService(5*time.Minute, func(svc *v1.Service) {
			svc.Spec.Ports = []v1.ServicePort{{Protocol: v1.ProtocolUDP, Port: 8082, TargetPort: intstr.FromInt(8081)}}
			svc.Spec.Selector = labels
		})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Checks from openshift perspective")
		o.Expect(svc.GetAnnotations()["loadbalancer.openstack.org/load-balancer-id"]).ShouldNot(o.BeEmpty(),
			"load-balancer-id annotation missing")
		loadBalancerId := svc.GetAnnotations()["loadbalancer.openstack.org/load-balancer-id"]
		o.Expect(svc.Status.LoadBalancer.Ingress[0].IP).ShouldNot(o.BeEmpty(), "FIP missing on svc Status")
		o.Expect(svc.Spec.ExternalTrafficPolicy).Should(o.Equal(v1.ServiceExternalTrafficPolicyTypeCluster),
			"Unexpected ExternalTrafficPolicy on svc specs")

		g.By("Checks from openstack perspective")
		lb, err := octavialoadbalancers.Get(loadBalancerClient, loadBalancerId).Extract()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(lb.Provider).Should(o.Equal("amphora"), "Unexpected provider in the Openstack LoadBalancer")

		svcIp := svc.Status.LoadBalancer.Ingress[0].IP
		svcPort := fmt.Sprint(svc.Spec.Ports[0].Port)
		fip, err := getFipbyFixedIP(networkClient, lb.VipAddress)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(fip.FloatingIP).Should(o.Equal(svcIp), "Unexpected floatingIp in the Openstack LoadBalancer")
		o.Expect(lb.Pools).Should(o.HaveLen(1), "Unexpected number of pools on Openstack LoadBalancer %q", lb.Name)

		pool, err := pools.Get(loadBalancerClient, lb.Pools[0].ID).Extract()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(pool.Protocol).Should(o.Equal("UDP"), "Unexpected protocol on Openstack LoadBalancer Pool: %q", pool.Name)
		//Set as OFFLINE in vexxhost despite the lb is operative
		//o.Expect(pool.OperatingStatus).Should(o.Equal("ONLINE"), "Unexpected Operating Status on Openstack Pool: %q", pool.Name)
		lbMethod, err := getClusterLoadBalancerSetting("lb-method", cloudProviderConfig)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(strings.ToLower(pool.LBMethod)).Should(o.Equal(lbMethod), "Unexpected LBMethod on Openstack Pool: %q", strings.ToLower(pool.LBMethod))

		g.By("accessing the service 100 times from outside and storing the name of the pods answering")
		results := make(map[string]int)
		for i := 0; i < 100; i++ {
			// https://github.com/kubernetes/kubernetes/blob/master/test/images/agnhost/README.md#netexec
			podName, err := getPodNameThroughUdpLb(svcIp, svcPort, "hostname")
			if err != nil {
				e2e.Logf("Error detected while accessing the LoadBalancer service on try %d: %q", i, err)
			} else {
				results[podName]++
			}
		}
		e2e.Logf("Pods accessed after 100 UDP requests:\n%v\n", results)
		pods, err := oc.KubeClient().CoreV1().Pods(oc.Namespace()).List(context.Background(), metav1.ListOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(isLbMethodApplied(lbMethod, results, pods)).Should(o.BeTrue(), "%q lb-method not applied after 100 queries:\n%v\n", lbMethod, results)
	})
})

// get the LoadBalancer setting based on the provided CloudProviderConfig INI file and the default values
func getClusterLoadBalancerSetting(setting string, config *ini.File) (string, error) {

	defaultLoadBalancerSettings := map[string]string{
		"lb-provider": "amphora",
		"lb-method":   "round_robin",
	}

	result, err := getPropertyValue("LoadBalancer", setting, config)
	if err != nil || result == "#UNDEFINED#" {
		if _, ok := defaultLoadBalancerSettings[setting]; !ok {
			return "", fmt.Errorf("%q setting value not found and default is unknown", setting)
		}
		result = defaultLoadBalancerSettings[setting]
		e2e.Logf("%q is not set on LoadBalancer section in cloud-provider-config, considering default value %q", setting, result)
	}
	return strings.ToLower(result), nil
}

// Return the FloatingIP assigned to a provided IP and return error if it is not found.
func getFipbyFixedIP(client *gophercloud.ServiceClient, vip string) (floatingips.FloatingIP, error) {
	var result floatingips.FloatingIP
	allPages, err := floatingips.List(client, floatingips.ListOpts{}).AllPages()
	if err != nil {
		return result, err
	}
	allFIP, err := floatingips.ExtractFloatingIPs(allPages)
	if err != nil {
		return result, err
	}
	for _, fip := range allFIP {
		if fip.FixedIP == vip {
			return fip, nil
		}
	}
	return result, fmt.Errorf("FIP not found for VIP %q", vip)
}

// Send message on provided ip and UDP port and return the answer.
// Error if there is no answer or cannot access the port after 5 seconds.
func getPodNameThroughUdpLb(ip string, port string, message string) (string, error) {

	service := ip + ":" + port
	RemoteAddr, err := net.ResolveUDPAddr("udp", service)
	if err != nil {
		return "", err
	}
	conn, err := net.DialUDP("udp", nil, RemoteAddr)
	if err != nil {
		return "", err
	}
	conn.SetDeadline(time.Now().Add(time.Second * 5))
	defer conn.Close()

	msg := []byte(message)
	_, err = conn.Write(msg)
	if err != nil {
		return "", err
	}

	// receive message from server
	buffer := make([]byte, 1024)
	n, _, err := conn.ReadFromUDP(buffer)
	if err != nil {
		return "", err
	}
	return string(buffer[:n]), nil
}

// Check if the provided lbMethod is applied considering the
// result map (<pod name>: <number of accesses>) and the list of pods provided.
// TO DO: Only 'round_robin' method implement for the moment.
func isLbMethodApplied(lbMethod string, results map[string]int, pods *v1.PodList) bool {

	safeguard := 20
	if lbMethod == "round_robin" {
		minAccesses := int(100/len(pods.Items) - safeguard)
		for _, pod := range pods.Items {
			e2e.Logf("Checking if pod %q has been accessed at least %v number of times...", pod.Name, minAccesses)
			val, ok := results[pod.Name]
			if !ok || val < minAccesses {
				return false
			}
		}
		return true
	} else {
		e2e.Logf("LB-Method check not implemented for %q", lbMethod)
		return true
	}
}
