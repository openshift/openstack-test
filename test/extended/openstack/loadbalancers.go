package openstack

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	octavialoadbalancers "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	octaviamonitors "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	g "github.com/onsi/ginkgo/v2"
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
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	imageutils "k8s.io/kubernetes/test/utils/image"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack][lb] The Openstack platform", func() {

	oc := exutil.NewCLI("openstack")
	var loadBalancerClient *gophercloud.ServiceClient
	var networkClient *gophercloud.ServiceClient
	var clientSet *kubernetes.Clientset
	var cloudProviderConfig *ini.File
	availableLbProvidersUnderTests := [2]string{"Amphora", "OVN"}

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

		g.By("Gathering cloud-provider-config")
		cloudProviderConfig, err = getConfig(oc.AdminKubeClient(),
			"openshift-config",
			"cloud-provider-config",
			"config")
		o.Expect(err).NotTo(o.HaveOccurred())

	})

	for _, i := range availableLbProvidersUnderTests {
		lbProviderUnderTest := i

		// https://issues.redhat.com/browse/OSASINFRA-2753
		g.It(fmt.Sprintf("should create an UDP %s LoadBalancer when an UDP svc with type:LoadBalancer is created on Openshift", lbProviderUnderTest), func() {

			skipIfNotLbProvider(lbProviderUnderTest, cloudProviderConfig)

			g.By("Creating Openshift deployment")
			labels := map[string]string{"app": "udp-lb-default-dep"}
			testDeployment := createTestDeployment("udp-lb-default-dep", labels, 2, v1.ProtocolUDP, 8081)
			deployment, err := clientSet.AppsV1().Deployments(oc.Namespace()).Create(context.TODO(),
				testDeployment, metav1.CreateOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			err = e2edeployment.WaitForDeploymentComplete(clientSet, deployment)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Creating Openshift LoadBalancer Service")
			svcName := "udp-lb-default-svc"
			svcPort := int32(8082)
			jig := e2eservice.NewTestJig(clientSet, oc.Namespace(), svcName)
			jig.Labels = labels
			svc, err := jig.CreateLoadBalancerService(5*time.Minute, func(svc *v1.Service) {
				svc.Spec.Ports = []v1.ServicePort{{Protocol: v1.ProtocolUDP, Port: svcPort, TargetPort: intstr.FromInt(8081)}}
				svc.Spec.Selector = labels
			})
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Checks from openshift perspective")
			loadBalancerId := svc.GetAnnotations()["loadbalancer.openstack.org/load-balancer-id"]
			o.Expect(loadBalancerId).ShouldNot(o.BeEmpty(), "load-balancer-id annotation missing")
			o.Expect(svc.Status.LoadBalancer.Ingress).ShouldNot(o.BeEmpty(), "svc.Status.LoadBalancer.Ingress should not be empty")
			svcIp := svc.Status.LoadBalancer.Ingress[0].IP
			o.Expect(svcIp).ShouldNot(o.BeEmpty(), "FIP missing on svc Status")
			o.Expect(svc.Spec.ExternalTrafficPolicy).Should(o.Equal(v1.ServiceExternalTrafficPolicyTypeCluster),
				"Unexpected ExternalTrafficPolicy on svc specs")

			g.By("Checks from openstack perspective")
			lb, err := octavialoadbalancers.Get(loadBalancerClient, loadBalancerId).Extract()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(lb.Provider).Should(o.Equal(strings.ToLower(lbProviderUnderTest)), "Unexpected provider in the Openstack LoadBalancer")

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
				podName, err := getPodNameThroughUdpLb(svcIp, fmt.Sprintf("%d", svcPort), "hostname")
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

		// https://github.com/kubernetes/cloud-provider-openstack/blob/master/docs/openstack-cloud-controller-manager/expose-applications-using-loadbalancer-type-service.md#sharing-load-balancer-with-multiple-services
		g.It(fmt.Sprintf("should re-use an existing UDP %s LoadBalancer when new svc is created on Openshift with the proper annotation", lbProviderUnderTest), func() {

			skipIfNotLbProvider(lbProviderUnderTest, cloudProviderConfig)

			g.By("Checking cluster configuration")
			setting, err := getClusterLoadBalancerSetting("max-shared-lb", cloudProviderConfig)
			o.Expect(err).NotTo(o.HaveOccurred())
			maxSharedLb, err := strconv.Atoi(setting)
			o.Expect(err).NotTo(o.HaveOccurred())
			if 2 < maxSharedLb {
				e2eskipper.Skipf("Test not applicable when max-shared-lb is lower than 2 and it is %d", maxSharedLb)
			}

			g.By("Creating Openshift deployment")
			labels := map[string]string{"app": "udp-lb-shared-dep"}
			testDeployment := createTestDeployment("udp-lb-shared-dep", labels, 2, v1.ProtocolUDP, 8081)
			deployment, err := clientSet.AppsV1().Deployments(oc.Namespace()).Create(context.TODO(),
				testDeployment, metav1.CreateOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			err = e2edeployment.WaitForDeploymentComplete(clientSet, deployment)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Creating first Openshift service")
			svcName1 := "udp-lb-shared1-svc"
			svcPort1 := int32(8082)
			jig := e2eservice.NewTestJig(clientSet, oc.Namespace(), svcName1)
			jig.Labels = labels
			svc1, err := jig.CreateLoadBalancerService(5*time.Minute, func(svc *v1.Service) {
				svc.Spec.Ports = []v1.ServicePort{{Protocol: v1.ProtocolUDP, Port: svcPort1, TargetPort: intstr.FromInt(8081)}}
				svc.Spec.Selector = labels
			})
			o.Expect(err).NotTo(o.HaveOccurred())
			loadBalancerId := svc1.GetAnnotations()["loadbalancer.openstack.org/load-balancer-id"]
			o.Expect(loadBalancerId).ShouldNot(o.BeEmpty(), "load-balancer-id annotation missing")
			e2e.Logf("detected loadbalancer id is %q", loadBalancerId)

			g.By(fmt.Sprintf("Creating second Openshift service using the existing Openstack loadbalancer %s", loadBalancerId))
			svcName2 := "udp-lb-shared2-svc"
			svcPort2 := int32(8083)
			jig = e2eservice.NewTestJig(clientSet, oc.Namespace(), svcName2)
			jig.Labels = labels
			svc2, err := jig.CreateLoadBalancerService(5*time.Minute, func(svc *v1.Service) {
				svc.Spec.Ports = []v1.ServicePort{{Protocol: v1.ProtocolUDP, Port: svcPort2, TargetPort: intstr.FromInt(8081)}}
				svc.Spec.Selector = labels
				svc.SetAnnotations(map[string]string{"loadbalancer.openstack.org/load-balancer-id": loadBalancerId})
			})
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Checks from openstack perspective")
			lb, err := octavialoadbalancers.Get(loadBalancerClient, loadBalancerId).Extract()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(lb.Pools).Should(o.HaveLen(2), "Unexpected number of pools on Openstack LoadBalancer %q", lb.Name)

			g.By("Connectivity checks")
			o.Expect(svc1.Status.LoadBalancer.Ingress).ShouldNot(o.BeEmpty(), "svc1.Status.LoadBalancer.Ingress should not be empty")
			o.Expect(svc2.Status.LoadBalancer.Ingress).ShouldNot(o.BeEmpty(), "svc2.Status.LoadBalancer.Ingress should not be empty")
			o.Expect(svc1.Status.LoadBalancer.Ingress[0].IP).Should(o.Equal(svc2.Status.LoadBalancer.Ingress[0].IP),
				"Unexpected external IP for svcs sharing the loadbalancer")
			fip := svc1.Status.LoadBalancer.Ingress[0].IP
			podNames, err := exutil.GetPodNamesByFilter(oc.KubeClient().CoreV1().Pods(oc.Namespace()),
				exutil.ParseLabelsOrDie(""), func(v1.Pod) bool { return true })
			o.Expect(err).NotTo(o.HaveOccurred())
			for _, svcPort := range [2]int32{svcPort1, svcPort2} {
				podName, err := getPodNameThroughUdpLb(fip, fmt.Sprintf("%d", svcPort), "hostname")
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(podName).Should(o.BeElementOf(podNames))
				e2e.Logf("Pod %s successfully accessed through svc loadbalancer on ip %s and port %d", podName, fip, svcPort)
			}
		})

		// https://issues.redhat.com/browse/OCPBUGS-2350
		g.It(fmt.Sprintf("should apply lb-method on UDP %s LoadBalancer when an UDP svc with monitors and ETP:Local is created on Openshift", lbProviderUnderTest), func() {

			skipIfNotLbProvider(lbProviderUnderTest, cloudProviderConfig)

			g.By("Creating Openshift deployment")
			labels := map[string]string{"app": "udp-lb-etplocal-dep"}
			testDeployment := createTestDeployment("udp-lb-etplocal-dep", labels, 2, v1.ProtocolUDP, 8081)
			deployment, err := clientSet.AppsV1().Deployments(oc.Namespace()).Create(context.TODO(),
				testDeployment, metav1.CreateOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			err = e2edeployment.WaitForDeploymentComplete(clientSet, deployment)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Creating Openshift LoadBalancer Service with monitors and ETP:Local")
			svcName := "udp-lb-etplocal-svc"
			svcPort := int32(8082)
			monitorDelay := 10
			monitorTimeout := 20
			monitorMaxRetries := 2
			jig := e2eservice.NewTestJig(clientSet, oc.Namespace(), svcName)
			jig.Labels = labels
			svc, err := jig.CreateLoadBalancerService(5*time.Minute, func(svc *v1.Service) {
				svc.Spec.Ports = []v1.ServicePort{{Protocol: v1.ProtocolUDP, Port: svcPort, TargetPort: intstr.FromInt(8081)}}
				svc.Spec.Selector = labels
				svc.SetAnnotations(map[string]string{
					"loadbalancer.openstack.org/enable-health-monitor":      "true",
					"loadbalancer.openstack.org/health-monitor-delay":       fmt.Sprintf("%d", monitorDelay),
					"loadbalancer.openstack.org/health-monitor-timeout":     fmt.Sprintf("%d", monitorTimeout),
					"loadbalancer.openstack.org/health-monitor-max-retries": fmt.Sprintf("%d", monitorMaxRetries),
				})
				svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
			})
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Checks from openshift perspective")
			loadBalancerId := svc.GetAnnotations()["loadbalancer.openstack.org/load-balancer-id"]
			o.Expect(loadBalancerId).ShouldNot(o.BeEmpty(), "load-balancer-id annotation missing")
			svcIp := svc.Status.LoadBalancer.Ingress[0].IP
			o.Expect(svcIp).ShouldNot(o.BeEmpty(), "FIP missing on svc Status")
			o.Expect(svc.Spec.ExternalTrafficPolicy).Should(o.Equal(v1.ServiceExternalTrafficPolicyTypeLocal),
				"Unexpected ExternalTrafficPolicy on svc specs")

			g.By("Checks from openstack perspective")
			lb, err := octavialoadbalancers.Get(loadBalancerClient, loadBalancerId).Extract()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(lb.Provider).Should(o.Equal(strings.ToLower(lbProviderUnderTest)), "Unexpected provider in the Openstack LoadBalancer")
			pool, err := pools.Get(loadBalancerClient, lb.Pools[0].ID).Extract()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(pool.Protocol).Should(o.Equal("UDP"), "Unexpected protocol on Openstack LoadBalancer Pool: %q", pool.Name)
			monitor, err := octaviamonitors.Get(loadBalancerClient, pool.MonitorID).Extract()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(monitor.AdminStateUp).Should(o.BeTrue(), "Unexpected healthmonitor adminStateUp on Openstack LoadBalancer Pool: %q", pool.Name)
			o.Expect(monitor.Delay).Should(o.Equal(monitorDelay), "Unexpected healthmonitor delay on Openstack LoadBalancer Pool: %q", pool.Name)
			o.Expect(monitor.Timeout).Should(o.Equal(monitorTimeout), "Unexpected healthmonitor timeout on Openstack LoadBalancer Pool: %q", pool.Name)
			o.Expect(monitor.MaxRetries).Should(o.Equal(monitorMaxRetries), "Unexpected healthmonitor MaxRetries on Openstack LoadBalancer Pool: %q", pool.Name)
			lbMethod, err := getClusterLoadBalancerSetting("lb-method", cloudProviderConfig)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(strings.ToLower(pool.LBMethod)).Should(o.Equal(lbMethod), "Unexpected LBMethod on Openstack Pool: %q", pool.LBMethod)

			g.By("accessing the service 100 times from outside and storing the name of the pods answering")
			results := make(map[string]int)
			for i := 0; i < 100; i++ {
				// https://github.com/kubernetes/kubernetes/blob/master/test/images/agnhost/README.md#netexec
				podName, err := getPodNameThroughUdpLb(svcIp, fmt.Sprintf("%d", svcPort), "hostname")
				if err != nil {
					e2e.Logf("Error detected while accessing the LoadBalancer service on try %d: %q", i, err)
				} else {
					results[podName]++
				}
			}
			e2e.Logf("Pods accessed after 100 UDP requests:\n%v\n", results)
			pods, err := oc.KubeClient().CoreV1().Pods(oc.Namespace()).List(context.Background(), metav1.ListOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			//lbMethod can be something different to ROUND_ROBIN as monitors && ETP:Local are enabled on this test:
			o.Expect(isLbMethodApplied(lbMethod, results, pods)).Should(o.BeTrue(), "%q lb-method not applied after 100 queries:\n%v\n", lbMethod, results)
		})
	}
})

// Check ini.File from cloudProviderConfig and skip the tests if lb-provider is not matching the expected value
func skipIfNotLbProvider(expectedLbProvider string, ini *ini.File) {

	foundLbProvider, err := getClusterLoadBalancerSetting("lb-provider", ini)
	o.Expect(err).NotTo(o.HaveOccurred())
	if foundLbProvider != strings.ToLower(expectedLbProvider) {
		e2eskipper.Skipf("Test not applicable for LoadBalancer provider different than %s. Cluster is configured with %q", expectedLbProvider, foundLbProvider)
	}
}

// get the LoadBalancer setting based on the provided CloudProviderConfig INI file and the default values
func getClusterLoadBalancerSetting(setting string, config *ini.File) (string, error) {

	defaultLoadBalancerSettings := map[string]string{
		"lb-provider":   "amphora",
		"lb-method":     "round_robin",
		"max-shared-lb": "2",
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
func isLbMethodApplied(lbMethod string, results map[string]int, pods *v1.PodList) bool {

	safeguard := 20
	// https://github.com/openstack/octavia/blob/master/releasenotes/notes/add-lb-algorithm-source-ip-port-ff86433143e43136.yaml
	if lbMethod == "round_robin" || lbMethod == "source_ip_port" {
		minAccesses := int(100/len(pods.Items) - safeguard)
		for _, pod := range pods.Items {
			e2e.Logf("Checking if pod %q has been accessed at least %v number of times...", pod.Name, minAccesses)
			val, ok := results[pod.Name]
			if !ok || val < minAccesses {
				return false
			}
		}
		return true
	} else if lbMethod == "source_ip" || lbMethod == "least_connections" {
		minAccesses := 100 - safeguard
		for _, pod := range pods.Items {
			val, ok := results[pod.Name]
			if ok && val > minAccesses && len(results) == 1 {
				e2e.Logf("%q algorithm successfully applied - Unique pod %q has been accessed %d times.", lbMethod, pod.Name, val)
				return true
			}
		}
		return false
	} else {
		e2e.Logf("LB-Method check not implemented for %q", lbMethod)
		return true
	}
}

// Creates *appsv1.Deployment using the image agnhost configuring a server on the specified port and protocol
// Further info on: https://github.com/kubernetes/kubernetes/blob/master/test/images/agnhost/README.md#netexec
func createTestDeployment(depName string, labels map[string]string, replicas int32, protocol v1.Protocol, port int32) *appsv1.Deployment {

	isFalse := false
	isTrue := true
	netExecParams := "--" + strings.ToLower(string(protocol)) + "-port=" + fmt.Sprintf("%d", port)

	testDeployment := e2edeployment.NewDeployment(depName, replicas, labels, "test",
		imageutils.GetE2EImage(imageutils.Agnhost), appsv1.RollingUpdateDeploymentStrategyType)
	testDeployment.Spec.Template.Spec.SecurityContext = &v1.PodSecurityContext{}
	testDeployment.Spec.Template.Spec.Containers[0].SecurityContext = &v1.SecurityContext{
		Capabilities:             &v1.Capabilities{Drop: []v1.Capability{"ALL"}},
		AllowPrivilegeEscalation: &isFalse,
		RunAsNonRoot:             &isTrue,
		SeccompProfile:           &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault},
	}
	testDeployment.Spec.Template.Spec.Containers[0].Args = []string{"netexec", netExecParams}
	testDeployment.Spec.Template.Spec.Containers[0].Ports = []v1.ContainerPort{{
		ContainerPort: port,
		Protocol:      protocol,
	}}
	return testDeployment
}
