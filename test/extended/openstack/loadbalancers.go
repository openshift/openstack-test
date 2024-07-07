package openstack

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	octavialisteners "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/listeners"
	octavialoadbalancers "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	octaviamonitors "github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	operatorv1 "github.com/openshift/api/operator/v1"
	exutil "github.com/openshift/origin/test/extended/util"
	ini "gopkg.in/ini.v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2edeployment "k8s.io/kubernetes/test/e2e/framework/deployment"
	e2eevents "k8s.io/kubernetes/test/e2e/framework/events"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	imageutils "k8s.io/kubernetes/test/utils/image"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform", func() {
	var ctx context.Context

	oc := exutil.NewCLI("openstack")
	var loadBalancerClient *gophercloud.ServiceClient
	var networkClient *gophercloud.ServiceClient
	var clientSet *kubernetes.Clientset
	var cloudProviderConfig *ini.File
	var loadBalancerServiceTimeout = 10 * time.Minute
	availableLbProvidersUnderTests := [2]string{"Amphora", "OVN"}
	availableProtocolsUnderTests := [2]v1.Protocol{v1.ProtocolTCP, v1.ProtocolUDP}
	lbMethodsWithETPGlobal := map[string]string{
		"OVN":     "source_ip_port",
		"Amphora": "round_robin",
	}

	g.BeforeEach(func() {
		ctx = context.Background()

		network, err := oc.AdminConfigClient().ConfigV1().Networks().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		if network.Status.NetworkType == "Kuryr" {
			e2eskipper.Skipf("Test not applicable for Kuryr NetworkType")
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

		for _, j := range availableProtocolsUnderTests {
			protocolUnderTest := j

			// https://issues.redhat.com/browse/OSASINFRA-2753
			// https://issues.redhat.com/browse/OSASINFRA-2412
			g.It(fmt.Sprintf("should create a %s %s LoadBalancer when a %s svc with type:LoadBalancer is created on Openshift", protocolUnderTest, lbProviderUnderTest, protocolUnderTest), func() {

				skipIfNotLbProvider(lbProviderUnderTest, cloudProviderConfig)

				g.By("Creating Openshift deployment")
				labels := map[string]string{"app": "lb-default-dep"}
				testDeployment := createTestDeployment("lb-default-dep", labels, 2, protocolUnderTest, 8081)
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
				o.Expect(pool.Protocol).Should(o.Equal(string(protocolUnderTest)), "Unexpected protocol on Openstack LoadBalancer Pool: %q", pool.Name)
				//Set as OFFLINE in vexxhost despite the lb is operative
				//o.Expect(pool.OperatingStatus).Should(o.Equal("ONLINE"), "Unexpected Operating Status on Openstack Pool: %q", pool.Name)
				lbMethod, err := GetClusterLoadBalancerSetting("lb-method", cloudProviderConfig)
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(strings.ToLower(pool.LBMethod)).Should(o.Equal(lbMethod), "Unexpected LBMethod on Openstack Pool: %q", strings.ToLower(pool.LBMethod))
				nodeList, err := clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
				o.Expect(err).NotTo(o.HaveOccurred())
				expectedNumberOfMembers := len(nodeList.Items)
				o.Expect(waitUntilNmembersReady(loadBalancerClient, pool, expectedNumberOfMembers, "NO_MONITOR|ONLINE")).NotTo(o.HaveOccurred(),
					"Error waiting for %d members with NO_MONITOR or ONLINE OperatingStatus", len(nodeList.Items))

				g.By("accessing the service 100 times from outside and storing the name of the pods answering")
				results := make(map[string]int)
				for i := 0; i < 100; i++ {
					// https://github.com/kubernetes/kubernetes/blob/master/test/images/agnhost/README.md#netexec
					podName, err := getPodNameThroughLb(svcIp, fmt.Sprintf("%d", svcPort), protocolUnderTest, "hostname")
					if err != nil {
						e2e.Logf("Error detected while accessing the LoadBalancer service on try %d: %q", i, err)
					} else {
						results[podName]++
					}
				}
				e2e.Logf("Pods accessed after 100 requests:\n%v\n", results)
				pods, err := oc.KubeClient().CoreV1().Pods(oc.Namespace()).List(ctx, metav1.ListOptions{})
				o.Expect(err).NotTo(o.HaveOccurred())

				// ETP:Local not configured so the defined lbMethod is not applied, but the one on 'lbMethodsWithETPGlobal' var.
				// https://issues.redhat.com/browse/OCPBUGS-2350
				o.Expect(isLbMethodApplied(lbMethodsWithETPGlobal[lbProviderUnderTest], results, pods)).Should(o.BeTrue(), "%q lb-method not applied after 100 queries:\n%v\n", lbMethod, results)
			})
		}

		// https://github.com/kubernetes/cloud-provider-openstack/blob/master/docs/openstack-cloud-controller-manager/expose-applications-using-loadbalancer-type-service.md#sharing-load-balancer-with-multiple-services
		g.It(fmt.Sprintf("should re-use an existing UDP %s LoadBalancer when new svc is created on Openshift with the proper annotation", lbProviderUnderTest), func() {

			skipIfNotLbProvider(lbProviderUnderTest, cloudProviderConfig)

			g.By("Checking cluster configuration")
			setting, err := GetClusterLoadBalancerSetting("max-shared-lb", cloudProviderConfig)
			o.Expect(err).NotTo(o.HaveOccurred())
			maxSharedLb, err := strconv.Atoi(setting)
			o.Expect(err).NotTo(o.HaveOccurred())
			if maxSharedLb < 2 {
				e2eskipper.Skipf("Test not applicable when max-shared-lb is lower than 2 and it is %d", maxSharedLb)
			}

			g.By("Creating Openshift deployment")
			labels := map[string]string{"app": "udp-lb-shared-dep"}
			testDeployment := createTestDeployment("udp-lb-shared-dep", labels, 2, v1.ProtocolUDP, 8081)
			deployment, err := clientSet.AppsV1().Deployments(oc.Namespace()).Create(ctx,
				testDeployment, metav1.CreateOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			err = e2edeployment.WaitForDeploymentComplete(clientSet, deployment)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Creating first Openshift service")
			svcName1 := "udp-lb-shared1-svc"
			svcPort1 := int32(8082)
			jig := e2eservice.NewTestJig(clientSet, oc.Namespace(), svcName1)
			jig.Labels = labels
			svc1, err := jig.CreateLoadBalancerService(ctx, loadBalancerServiceTimeout, func(svc *v1.Service) {
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
			svc2, err := jig.CreateLoadBalancerService(ctx, loadBalancerServiceTimeout, func(svc *v1.Service) {
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

			maxAttempts := 100
			podName := ""
			for _, svcPort := range [2]int32{svcPort1, svcPort2} {
				for count := 0; count < maxAttempts; count++ {
					podName, err = getPodNameThroughLb(fip, fmt.Sprintf("%d", svcPort), v1.ProtocolUDP, "hostname")
					if err == nil {
						break
					}
				}
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(podName).Should(o.BeElementOf(podNames))
				e2e.Logf("Pod %s successfully accessed through svc loadbalancer on ip %s and port %d", podName, fip, svcPort)
			}
		})

		// https://github.com/kubernetes/cloud-provider-openstack/blob/master/docs/openstack-cloud-controller-manager/expose-applications-using-loadbalancer-type-service.md#sharing-load-balancer-with-multiple-services
		// In order to prevent accidental exposure internal Services cannot share a load balancer with any other Service.
		g.It(fmt.Sprintf("should not re-use an existing UDP %s LoadBalancer if shared internal svc is created", lbProviderUnderTest), func() {

			skipIfNotLbProvider(lbProviderUnderTest, cloudProviderConfig)

			g.By("Checking cluster configuration")
			setting, err := GetClusterLoadBalancerSetting("max-shared-lb", cloudProviderConfig)
			o.Expect(err).NotTo(o.HaveOccurred())
			maxSharedLb, err := strconv.Atoi(setting)
			o.Expect(err).NotTo(o.HaveOccurred())
			if maxSharedLb < 2 {
				e2eskipper.Skipf("Test not applicable when max-shared-lb is lower than 2 and it is %d", maxSharedLb)
			}

			g.By("Creating Openshift deployment")
			labels := map[string]string{"app": "udp-lb-shared-dep"}
			testDeployment := createTestDeployment("udp-lb-shared-dep", labels, 2, v1.ProtocolUDP, 8081)
			deployment, err := clientSet.AppsV1().Deployments(oc.Namespace()).Create(ctx,
				testDeployment, metav1.CreateOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			err = e2edeployment.WaitForDeploymentComplete(clientSet, deployment)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Creating first Openshift service")
			svcName1 := "udp-lb-shared1-svc"
			svcPort1 := int32(8082)
			jig := e2eservice.NewTestJig(clientSet, oc.Namespace(), svcName1)
			jig.Labels = labels
			svc1, err := jig.CreateLoadBalancerService(ctx, loadBalancerServiceTimeout, func(svc *v1.Service) {
				svc.Spec.Ports = []v1.ServicePort{{Protocol: v1.ProtocolUDP, Port: svcPort1, TargetPort: intstr.FromInt(8081)}}
				svc.Spec.Selector = labels
			})
			o.Expect(err).NotTo(o.HaveOccurred())
			loadBalancerId := svc1.GetAnnotations()["loadbalancer.openstack.org/load-balancer-id"]
			o.Expect(loadBalancerId).ShouldNot(o.BeEmpty(), "load-balancer-id annotation missing")
			e2e.Logf("detected loadbalancer id is %q", loadBalancerId)

			g.By(fmt.Sprintf("Creating internal Openshift service using the existing Openstack loadbalancer %s -"+
				"This should not be allowed.", loadBalancerId))
			svcName2 := "udp-lb-shared2-internal-svc"
			svcPort2 := int32(8083)
			jig = e2eservice.NewTestJig(clientSet, oc.Namespace(), svcName2)
			jig.Labels = labels
			svc2, err := jig.CreateLoadBalancerService(ctx, 1*time.Second, func(svc *v1.Service) {
				svc.Spec.Ports = []v1.ServicePort{{Protocol: v1.ProtocolUDP, Port: svcPort2, TargetPort: intstr.FromInt(8081)}}
				svc.Spec.Selector = labels
				svc.SetAnnotations(map[string]string{"loadbalancer.openstack.org/load-balancer-id": loadBalancerId,
					"service.beta.kubernetes.io/openstack-internal-load-balancer": "true"})
			})
			o.Expect(err).To(o.HaveOccurred()) //Error is expected
			o.Expect(svc2).To(o.BeNil())
			g.By("Waiting for event reporting the error creating the internal lb")
			eventSelector := fields.Set{
				"involvedObject.kind":      "Service",
				"involvedObject.name":      svcName2,
				"involvedObject.namespace": oc.Namespace(),
				"reason":                   "SyncLoadBalancerFailed",
			}.AsSelector().String()
			msg := "internal Service cannot share a load balancer"
			err = e2eevents.WaitTimeoutForEvent(ctx, clientSet, oc.Namespace(), eventSelector, msg, 100*time.Second)
			o.Expect(err).NotTo(o.HaveOccurred(), "Expected event not found.")
			e2e.Logf("Expected event found.")

			g.By("Checks from openstack perspective that there is still only one pool for the lb")
			lb, err := octavialoadbalancers.Get(loadBalancerClient, loadBalancerId).Extract()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(lb.Pools).Should(o.HaveLen(1), "Unexpected number of pools on Openstack LoadBalancer %q", lb.Name)
			e2e.Logf("Expected number of pools found on the openstack loadbalancer.")
		})

		for _, j := range availableProtocolsUnderTests {
			protocolUnderTest := j

			// https://issues.redhat.com/browse/OCPBUGS-2350
			g.It(fmt.Sprintf("should apply lb-method on %s %s LoadBalancer when a %s svc with monitors and ETP:Local is created on Openshift", protocolUnderTest, lbProviderUnderTest, protocolUnderTest), func() {

				skipIfNotLbProvider(lbProviderUnderTest, cloudProviderConfig)

				// Skip for OpenshiftSDN and Octavia version < 2.16 as Octavia UDP-CONNECT type health monitors
				// don't work with OpenShiftSDN (it incorrectly marks the nodes without local endpoints as ONLINE)
				// https://issues.redhat.com/browse/OCPBUGS-7229
				octaviaMinVersion := "v2.16"
				networkType, err := getNetworkType(ctx, oc)
				o.Expect(err).NotTo(o.HaveOccurred())

				octaviaGreaterOrEqualV2_16, err := IsOctaviaVersionGreaterThanOrEqual(loadBalancerClient, octaviaMinVersion)
				o.Expect(err).NotTo(o.HaveOccurred())

				if networkType == NetworkTypeOpenShiftSDN && !octaviaGreaterOrEqualV2_16 && protocolUnderTest == v1.ProtocolUDP {
					e2eskipper.Skipf("Test not applicable for %s network type when the Octavia version < %s", NetworkTypeOpenShiftSDN, octaviaMinVersion)
				}

				g.By("Creating Openshift deployment")
				labels := map[string]string{"app": "lb-etplocal-dep"}
				replicas := int32(2)
				testDeployment := createTestDeployment("lb-etplocal-dep", labels, replicas, protocolUnderTest, 8081)
				deployment, err := clientSet.AppsV1().Deployments(oc.Namespace()).Create(ctx,
					testDeployment, metav1.CreateOptions{})
				o.Expect(err).NotTo(o.HaveOccurred())
				err = e2edeployment.WaitForDeploymentComplete(clientSet, deployment)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Creating Openshift LoadBalancer Service with monitors and ETP:Local")
				svcName := "lb-etplocal-svc"
				svcPort := int32(8082)
				monitorDelay := 5
				monitorTimeout := 5
				monitorMaxRetries := 2
				jig := e2eservice.NewTestJig(clientSet, oc.Namespace(), svcName)
				jig.Labels = labels
				svc, err := jig.CreateLoadBalancerService(ctx, loadBalancerServiceTimeout, func(svc *v1.Service) {
					svc.Spec.Ports = []v1.ServicePort{{Protocol: protocolUnderTest, Port: svcPort, TargetPort: intstr.FromInt(8081)}}
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
				o.Expect(pool.Protocol).Should(o.Equal(string(protocolUnderTest)), "Unexpected protocol on Openstack LoadBalancer Pool: %q", pool.Name)
				monitor, err := octaviamonitors.Get(loadBalancerClient, pool.MonitorID).Extract()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(monitor.AdminStateUp).Should(o.BeTrue(), "Unexpected healthmonitor adminStateUp on Openstack LoadBalancer Pool: %q", pool.Name)
				o.Expect(monitor.Delay).Should(o.Equal(monitorDelay), "Unexpected healthmonitor delay on Openstack LoadBalancer Pool: %q", pool.Name)
				o.Expect(monitor.Timeout).Should(o.Equal(monitorTimeout), "Unexpected healthmonitor timeout on Openstack LoadBalancer Pool: %q", pool.Name)
				o.Expect(monitor.MaxRetries).Should(o.Equal(monitorMaxRetries), "Unexpected healthmonitor MaxRetries on Openstack LoadBalancer Pool: %q", pool.Name)
				lbMethod, err := GetClusterLoadBalancerSetting("lb-method", cloudProviderConfig)
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(strings.ToLower(pool.LBMethod)).Should(o.Equal(lbMethod), "Unexpected LBMethod on Openstack Pool: %q", pool.LBMethod)

				if monitor.Type == "UDP-CONNECT" {
					o.Expect(waitUntilNmembersReady(loadBalancerClient, pool, int(replicas), "ONLINE")).NotTo(o.HaveOccurred(),
						"Error waiting for %d members with ONLINE OperatingStatus", replicas)
				} else if monitor.Type == "HTTP" {
					// vexxhost uses HTTP healthmonitor and the OperatingStatus is not updated in that case.
					// Therefore, the test cannot know when the the members are ready, so waiting 30 seconds for stability
					e2e.Logf("monitor type is HTTP - Giving 30 seconds to let it calculate the ONLINE members")
					time.Sleep(30 * time.Second)
				}

				g.By("accessing the service 100 times from outside and storing the name of the pods answering")
				results := make(map[string]int)
				for i := 0; i < 100; i++ {
					// https://github.com/kubernetes/kubernetes/blob/master/test/images/agnhost/README.md#netexec
					podName, err := getPodNameThroughLb(svcIp, fmt.Sprintf("%d", svcPort), protocolUnderTest, "hostname")
					if err != nil {
						e2e.Logf("Error detected while accessing the LoadBalancer service on try %d: %q", i, err)
					} else {
						results[podName]++
					}
				}
				e2e.Logf("Pods accessed after 100 %s requests:\n%v\n", protocolUnderTest, results)
				pods, err := oc.KubeClient().CoreV1().Pods(oc.Namespace()).List(ctx, metav1.ListOptions{})
				o.Expect(err).NotTo(o.HaveOccurred())
				//lbMethod can be something different to ROUND_ROBIN as monitors && ETP:Local are enabled on this test:
				o.Expect(isLbMethodApplied(lbMethod, results, pods)).Should(o.BeTrue(), "%q lb-method not applied after 100 queries:\n%v\n", lbMethod, results)
			})
		}

		// https://bugzilla.redhat.com/show_bug.cgi?id=1997704
		g.It(fmt.Sprintf("should create an UDP %s LoadBalancer using a pre-created FIP when an UDP LoadBalancer svc setting the LoadBalancerIP spec is created on Openshift", lbProviderUnderTest), func() {

			skipIfNotLbProvider(lbProviderUnderTest, cloudProviderConfig)

			g.By("Create FIP to be used on the subsequent LoadBalancer Service")
			var fip *floatingips.FloatingIP
			foundNetworkId, err := GetFloatingNetworkID(networkClient, cloudProviderConfig)
			o.Expect(err).NotTo(o.HaveOccurred())
			createOpts := floatingips.CreateOpts{FloatingNetworkID: foundNetworkId}
			configuredSubnetId, _ := GetClusterLoadBalancerSetting("floating-subnet-id", cloudProviderConfig)
			if configuredSubnetId != "" {
				createOpts.SubnetID = configuredSubnetId
			}
			fip, err = floatingips.Create(networkClient, createOpts).Extract()
			o.Expect(err).NotTo(o.HaveOccurred(), "error creating FIP using IDs configured on the OCP cluster. Network-id: %s. Subnet-id: %s", foundNetworkId, configuredSubnetId)
			g.By(fmt.Sprintf("FIP created: %s", fip.FloatingIP))
			defer floatingips.Delete(networkClient, fip.ID)

			g.By("Creating Openshift deployment")
			depName := "udp-lb-precreatedfip-dep"
			svcName := "udp-lb-precreatedfip-svc"
			svcPort := int32(8022)
			labels := map[string]string{"app": depName}
			testDeployment := createTestDeployment(depName, labels, 2, v1.ProtocolUDP, 8081)
			deployment, err := clientSet.AppsV1().Deployments(oc.Namespace()).Create(ctx,
				testDeployment, metav1.CreateOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())
			err = e2edeployment.WaitForDeploymentComplete(clientSet, deployment)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By(fmt.Sprintf("Creating Openshift LoadBalancer Service using the FIP %s on network %s", fip.FloatingIP, fip.FloatingNetworkID))
			jig := e2eservice.NewTestJig(clientSet, oc.Namespace(), svcName)
			jig.Labels = labels
			svc, err := jig.CreateLoadBalancerService(ctx, loadBalancerServiceTimeout, func(svc *v1.Service) {
				svc.Spec.Ports = []v1.ServicePort{{Protocol: v1.ProtocolUDP, Port: svcPort, TargetPort: intstr.FromInt(8081)}}
				svc.Spec.Selector = labels
				svc.Spec.LoadBalancerIP = fip.FloatingIP
			})
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Checks from openshift perspective")
			o.Expect(svc.Status.LoadBalancer.Ingress[0].IP).Should(o.Equal(fip.FloatingIP))

			g.By("Connectivity checks")
			podNames, err := exutil.GetPodNamesByFilter(oc.KubeClient().CoreV1().Pods(oc.Namespace()),
				exutil.ParseLabelsOrDie(""), func(v1.Pod) bool { return true })
			o.Expect(err).NotTo(o.HaveOccurred())
			maxAttempts := 100 // Multiple connection attempts to stabilize the test in vexxhost
			var podName string
			for count := 0; count < maxAttempts; count++ {
				podName, err = getPodNameThroughLb(fip.FloatingIP, fmt.Sprintf("%d", svcPort), v1.ProtocolUDP, "hostname")
				if err == nil {
					count = maxAttempts
				}
			}
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(podName).Should(o.BeElementOf(podNames))
			e2e.Logf("Pod %s successfully accessed through svc loadbalancer on ip %s and port %d", podName, fip.FloatingIP, svcPort)
		})

		// https://issues.redhat.com/browse/OSASINFRA-2412
		g.It(fmt.Sprintf("should create a TCP %s LoadBalancer when LoadBalancerService ingressController is created on Openshift", lbProviderUnderTest), func() {

			skipIfNotLbProvider(lbProviderUnderTest, cloudProviderConfig)

			g.By("Creating ingresscontroller for sharding the OCP in-built canary service")
			name := "canary-sharding"
			domain := "sharding-test.apps.shiftstack.com"
			shardIngressCtrl, err := deployLbIngressController(oc, 10*time.Minute, name, domain, map[string]string{"ingress.openshift.io/canary": "canary_controller"})
			defer func() {
				err := oc.AdminOperatorClient().OperatorV1().IngressControllers(shardIngressCtrl.Namespace).Delete(context.Background(), shardIngressCtrl.Name, metav1.DeleteOptions{})
				o.Expect(err).NotTo(o.HaveOccurred(), "error destroying ingress-controller created during the test")
			}()
			o.Expect(err).NotTo(o.HaveOccurred(), "new ingresscontroller did not rollout")

			g.By("Checks from openshift perspective")
			svc, err := oc.AdminKubeClient().CoreV1().Services("openshift-ingress").Get(context.Background(), "router-"+name, metav1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred(), "router-%q not found in openshift-ingress namespace", name)
			o.Expect(svc.Spec.Type).Should(o.Equal(v1.ServiceTypeLoadBalancer), "Unexpected svc Type: %q", svc.Spec.Type)
			loadBalancerId := svc.GetAnnotations()["loadbalancer.openstack.org/load-balancer-id"]
			o.Expect(loadBalancerId).ShouldNot(o.BeEmpty(), "load-balancer-id annotation missing")
			o.Expect(svc.Status.LoadBalancer.Ingress).ShouldNot(o.BeEmpty(), "svc.Status.LoadBalancer.Ingress should not be empty")
			svcIp := svc.Status.LoadBalancer.Ingress[0].IP
			o.Expect(svcIp).ShouldNot(o.BeEmpty(), "FIP missing on svc Status")
			o.Expect(svc.Spec.ExternalTrafficPolicy).Should(o.Equal(v1.ServiceExternalTrafficPolicyTypeLocal),
				"Unexpected ExternalTrafficPolicy on svc specs")
			e2e.Logf("Service with LoadBalancerType exists with public ip %q and it is pointing to openstack loadbalancer with id %q", svcIp, loadBalancerId)

			g.By("Checks from openstack perspective")
			lb, err := octavialoadbalancers.Get(loadBalancerClient, loadBalancerId).Extract()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(lb.Provider).Should(o.Equal(strings.ToLower(lbProviderUnderTest)), "Unexpected provider in the Openstack LoadBalancer")
			//In canary, two pools are created: ports 80 and 443. Checking all existing pools:
			for i := 0; i < len(svc.Spec.Ports); i++ {
				poolId := lb.Pools[i].ID
				pool, err := pools.Get(loadBalancerClient, poolId).Extract()
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(pool.Protocol).Should(o.Equal("TCP"), "Unexpected protocol on Openstack LoadBalancer Pool: %q", pool.Name)
			}

			g.By("Test that the canary service is accessible through new ingressController")
			route, err := oc.AdminRouteClient().RouteV1().Routes("openshift-ingress-canary").Get(context.Background(), "canary", metav1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred(), "canary route not found")
			host := ""
			for _, ingress := range route.Status.Ingress {
				if strings.HasSuffix(ingress.Host, domain) {
					host = ingress.Host
				}
			}
			o.Expect(host).ShouldNot(o.Equal(""), "Host not found in route")
			for i := 0; i < 100; i++ {
				e2e.Logf("Attempting to connect using host: %q", host)
				resp, err := httpsGetWithCustomLookup(host, svcIp)
				o.Expect(err).NotTo(o.HaveOccurred())
				defer resp.Body.Close()
				o.Expect(resp.Status).Should(o.Equal("200 OK"), "Unexpected response on try #%d", i)
				body, err := io.ReadAll(resp.Body)
				o.Expect(err).NotTo(o.HaveOccurred())
				o.Expect(string(body)).Should(o.Equal("Healthcheck requested\n"), "Unexpected response on try #%d", i)
				o.Expect(fmt.Sprintf("%q", resp.TLS.PeerCertificates[0].Subject)).Should(o.Equal("\"CN=*."+domain+"\""), "Unexpected response on try #%d", i)
			}
			e2e.Logf("Canary service successfully accessed through new ingressController")
		})
	}

	// Only valid with the Octavia amphora provider. The OVN-Octavia provider does not
	// support allowed_cidrs in the LB (https://issues.redhat.com/browse/OCPBUGS-2789)
	g.It("should limit service access on an UDP Amphora LoadBalancer when an UDP LoadBalancer svc setting the loadBalancerSourceRanges spec is created on Openshift", func() {

		// Skipped for OVN-Octavia provider
		skipIfNotLbProvider("Amphora", cloudProviderConfig)

		g.By("Creating Openshift deployment")
		depName := "udp-lb-sourceranges-dep"
		svcPort := int32(8082)
		labels := map[string]string{"app": depName}
		drop_sourcerange := "9.9.9.9/32"
		testDeployment := createTestDeployment(depName, labels, int32(2), v1.ProtocolUDP, 8081)
		deployment, err := clientSet.AppsV1().Deployments(oc.Namespace()).Create(ctx,
			testDeployment, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		err = e2edeployment.WaitForDeploymentComplete(clientSet, deployment)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("Creating Openshift LoadBalancer Service with loadBalancerSourceRanges: '%s'", drop_sourcerange))
		jig := e2eservice.NewTestJig(clientSet, oc.Namespace(), "udp-lb-sourceranges-svc")
		jig.Labels = labels
		svc, err := jig.CreateLoadBalancerService(ctx, loadBalancerServiceTimeout, func(svc *v1.Service) {
			svc.Spec.Ports = []v1.ServicePort{{Protocol: v1.ProtocolUDP, Port: svcPort, TargetPort: intstr.FromInt(8081)}}
			svc.Spec.Selector = labels
			svc.Spec.LoadBalancerSourceRanges = []string{drop_sourcerange}
		})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Checks from openshift perspective")
		loadBalancerId := svc.GetAnnotations()["loadbalancer.openstack.org/load-balancer-id"]
		o.Expect(loadBalancerId).ShouldNot(o.BeEmpty(), "load-balancer-id annotation missing")
		e2e.Logf("LB ID: %s", loadBalancerId)
		svcIp := svc.Status.LoadBalancer.Ingress[0].IP
		o.Expect(svcIp).ShouldNot(o.BeEmpty(), "FIP missing on svc Status")
		e2e.Logf("Service IP: %s", svcIp)

		g.By("Checks from openstack perspective")
		allPages, err := octavialisteners.List(loadBalancerClient, octavialisteners.ListOpts{LoadbalancerID: loadBalancerId}).AllPages()
		o.Expect(err).NotTo(o.HaveOccurred())
		listeners, err := octavialisteners.ExtractListeners(allPages)
		o.Expect(err).NotTo(o.HaveOccurred())

		// Check there is only one listener in the LB
		o.Expect(listeners).Should(o.HaveLen(1), "Unexpected number of listeners for LB '%s'", loadBalancerId)
		listenerId := listeners[0].ID
		e2e.Logf("LB listener ID: %s", listenerId)

		// Check allowed_cidrs in the LB listener matches with the svc spec
		o.Expect(listeners[0].AllowedCIDRs).Should(o.ConsistOf([]string{drop_sourcerange}), "Unexpected allowed_cidrs in Openstack LoadBalancer Listener '%s'", listenerId)
		e2e.Logf("Found expected allowed_cidrs '%v' in Openstack LoadBalancer Listener '%s'", listeners[0].AllowedCIDRs, listenerId)

		// Check there is no connectivity
		g.By("Connectivity checks - there should be no connectivity")
		var connNumber int = 5
		g.By(fmt.Sprintf("Trying to access the service %d times from outside and storing the name of the pods answering", connNumber))
		results := make(map[string]int)
		for i := 0; i < connNumber; i++ {
			// https://github.com/kubernetes/kubernetes/blob/master/test/images/agnhost/README.md#netexec
			podName, err := getPodNameThroughLb(svcIp, fmt.Sprintf("%d", svcPort), v1.ProtocolUDP, "hostname")
			if err != nil {
				e2e.Logf("No connectivity (as expected) while accessing the LoadBalancer service on try %d: %q", i, err)
			} else {
				results[podName]++
			}
		}
		e2e.Logf("Pods accessed after '%d' UDP requests: '%v'", connNumber, results)
		o.Expect(len(results)).Should(o.Equal(0), "There is connectivity to svc pods when it shouldn't: '%v'", results)
		e2e.Logf("No connectivity to the service as expected")

		// Remove the LoadBalancerSourceRanges spec from the service
		_, err = jig.UpdateService(ctx, func(svc *v1.Service) {
			svc.Spec.LoadBalancerSourceRanges = nil
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("Removed LoadBalancerSourceRanges spec from the service")

		// Wait until allowed_cidrs in the LB listener is updated to 0.0.0.0/0 (all traffic allowed)
		allowAllAllowedCidrs := []string{"0.0.0.0/0"}
		e2e.Logf("Expected allowed_cidrs: '%v'", allowAllAllowedCidrs)
		o.Eventually(func() []string {
			lbListener, err := octavialisteners.Get(loadBalancerClient, listenerId).Extract()
			if err != nil {
				e2e.Logf("Error ocurred: %v, trying next iteration", err)
				return []string{}
			}
			e2e.Logf("Found AllowedCIDRs: %v", lbListener.AllowedCIDRs)
			return lbListener.AllowedCIDRs
		}, "10s", "1s").Should(o.ConsistOf(allowAllAllowedCidrs), "Didn't find the expected allowed_cidrs '%v'", allowAllAllowedCidrs)
		e2e.Logf("Found expected allowed_cidrs '%v' in Openstack LoadBalancer Listener '%s'", allowAllAllowedCidrs, listenerId)

		// Wait until ProvisioningStatus in the LB listener is back to ACTIVE after it's been updated
		activeProvisioningStatus := "ACTIVE"
		e2e.Logf("Expected ProvisioningStatus: '%s'", activeProvisioningStatus)
		o.Eventually(func() string {
			lbListener, err := octavialisteners.Get(loadBalancerClient, listenerId).Extract()
			if err != nil {
				e2e.Logf("Error ocurred: %v, trying next iteration", err)
				return ""
			}
			e2e.Logf("Found ProvisioningStatus: %s", lbListener.ProvisioningStatus)
			return lbListener.ProvisioningStatus
		}, "30s", "1s").Should(o.Equal(activeProvisioningStatus), "Didn't find the expected ProvisioningStatus '%s'", activeProvisioningStatus)
		e2e.Logf("Found expected ProvisioningStatus '%s' in Openstack LoadBalancer Listener '%s'", activeProvisioningStatus, listenerId)

		// Check there is still only one listener in the LB and that the Id matches with the previously stored one
		allPages, err = octavialisteners.List(loadBalancerClient, octavialisteners.ListOpts{LoadbalancerID: loadBalancerId}).AllPages()
		o.Expect(err).NotTo(o.HaveOccurred())
		listeners, err = octavialisteners.ExtractListeners(allPages)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(listeners).Should(o.HaveLen(1), "Unexpected number of listeners for LB '%s'", loadBalancerId)
		o.Expect(listeners[0].ID).Should((o.Equal(listenerId)), "Listener ID for LB '%s' has changed", loadBalancerId)

		// Check connectivity to the service
		connNumber = 10
		g.By(fmt.Sprintf("accessing the service %d times from outside and storing the name of the pods answering", connNumber))
		results = make(map[string]int)
		for i := 0; i < connNumber; i++ {
			// https://github.com/kubernetes/kubernetes/blob/master/test/images/agnhost/README.md#netexec
			podName, err := getPodNameThroughLb(svcIp, fmt.Sprintf("%d", svcPort), v1.ProtocolUDP, "hostname")
			if err != nil {
				e2e.Logf("Error detected while accessing the LoadBalancer service on try %d: %q", i, err)
			} else {
				results[podName]++
			}
		}
		e2e.Logf("Pods accessed after '%d' UDP requests: '%v'", connNumber, results)
		var successConnCount int
		for podName, connPerPod := range results {
			e2e.Logf("Pod '%s' has been accessed '%d' times", podName, connPerPod)
			successConnCount += connPerPod
		}

		// Check the number of successful connections with some margin (80%) to avoid flakes
		var connMargin float32 = 0.8
		minSuccessConn := int(float32(connNumber) * connMargin)
		o.Expect(successConnCount >= minSuccessConn).To(o.BeTrue(), "Found less successful connections (%d) than the minimum expected of '%d'", successConnCount, minSuccessConn)
		e2e.Logf("Found expected number of successfull connections: '%d'", connNumber)
	})
})

// Check ini.File from cloudProviderConfig and skip the tests if lb-provider is not matching the expected value
func skipIfNotLbProvider(expectedLbProvider string, ini *ini.File) {

	foundLbProvider, err := GetClusterLoadBalancerSetting("lb-provider", ini)
	o.Expect(err).NotTo(o.HaveOccurred())
	if foundLbProvider != strings.ToLower(expectedLbProvider) {
		e2eskipper.Skipf("Test not applicable for LoadBalancer provider different than %s. Cluster is configured with %q", expectedLbProvider, foundLbProvider)
	}
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

func getPodNameThroughLb(ip string, port string, protocol v1.Protocol, message string) (string, error) {

	if protocol == v1.ProtocolUDP {
		// Send message on provided ip and UDP port and return the answer.
		// Error if there is no answer or cannot access the port after 5 seconds.

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
		n, _, err := conn.ReadFrom(buffer)
		if err != nil {
			return "", err
		}
		return string(buffer[:n]), nil

	} else if protocol == v1.ProtocolTCP {
		// Send message on top of HTTP GET on provided ip and TCP port and return the answer.
		// Error if there is no answer or cannot access the port after 5 seconds.

		client := http.Client{
			Timeout: 5 * time.Second,
		}
		defer client.CloseIdleConnections()

		req, err := http.NewRequest("GET", "http://"+ip+":"+port+"/"+message, nil)
		if err != nil {
			return "", err
		}

		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}

		return string(body), nil

	} else {
		return "", fmt.Errorf("unsupported protocol %q", protocol)
	}
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

	var netExecParams string
	switch protocol {
	case v1.ProtocolTCP:
		netExecParams = fmt.Sprintf("--http-port=%d", port)
	default:
		netExecParams = fmt.Sprintf("--%s-port=%d", strings.ToLower(string(protocol)), port)
	}

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

// getSubnet checks if a Subnet is present in the list of Subnets the tenant has access to and returns it
func getSubnet(networkSubnet string, subnetList []subnets.Subnet) *subnets.Subnet {
	for _, subnet := range subnetList {
		if subnet.ID == networkSubnet {
			return &subnet
		}
	}
	return nil
}

// Active wait (up to 3 minutes) until a certain number of members match a regex on status attribute.
func waitUntilNmembersReady(client *gophercloud.ServiceClient, pool *pools.Pool, n int, status string) error {

	var resultingMembers []string
	var err error

	e2e.Logf("Waiting for %d members matching status %s in the pool %s", n, status, pool.Name)
	err = wait.Poll(15*time.Second, 3*time.Minute, func() (bool, error) {
		resultingMembers, err = getMembersMatchingStatus(client, pool, status)
		if err != nil {
			return false, err
		}
		if len(resultingMembers) == int(n) {
			e2e.Logf("Found expected number of %s members, checking again 5 seconds later to confirm that it is stable", status)
			time.Sleep(5 * time.Second)
			resultingMembers, err = getMembersMatchingStatus(client, pool, status)
			if err != nil {
				return false, err
			} else if len(resultingMembers) == int(n) {
				e2e.Logf("Wait completed, %s members found: %q", status, resultingMembers)
				return true, nil
			}
		}
		e2e.Logf("Waiting for next iteration, %s members found: %q", status, resultingMembers)
		return false, nil
	})

	if err != nil && strings.Contains(err.Error(), "timed out waiting for the condition") {
		resultingMembers, err = getMembersMatchingStatus(client, pool, status)
		allMembers, err := pools.ListMembers(client, pool.ID, pools.ListMembersOpts{}).AllPages()
		if err != nil {
			return err
		}
		return fmt.Errorf("expected %d members matching %s status, got %d. All members: %+v", n, status, len(resultingMembers), allMembers)
	}
	return err
}

// Return the name of the members matching a regex in the OperatingStatus attribute
func getMembersMatchingStatus(client *gophercloud.ServiceClient, pool *pools.Pool, status string) ([]string, error) {

	var members []string
	for _, aux := range pool.Members {
		member, err := pools.GetMember(client, pool.ID, aux.ID).Extract()
		if err != nil {
			return nil, err
		}
		result, err := regexp.MatchString(status, member.OperatingStatus)
		if err != nil {
			return nil, err
		} else if result {
			members = append(members, member.Name)
		}
	}
	return members, nil
}

// Deploy ingressController with Type "LoadBalancerService" and wait until it is ready
func deployLbIngressController(oc *exutil.CLI, timeout time.Duration, name, domain string, routeSelectorlabels map[string]string) (*operatorv1.IngressController, error) {

	var ingressControllerLbAvailableConditions = []operatorv1.OperatorCondition{
		{Type: operatorv1.IngressControllerAvailableConditionType, Status: operatorv1.ConditionTrue},
		{Type: operatorv1.LoadBalancerManagedIngressConditionType, Status: operatorv1.ConditionTrue},
		{Type: operatorv1.LoadBalancerReadyIngressConditionType, Status: operatorv1.ConditionTrue},
		{Type: "Admitted", Status: operatorv1.ConditionTrue},
	}
	ingressCtrl := &operatorv1.IngressController{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "openshift-ingress-operator",
		},
		Spec: operatorv1.IngressControllerSpec{
			Domain: domain,
			EndpointPublishingStrategy: &operatorv1.EndpointPublishingStrategy{
				Type: operatorv1.LoadBalancerServiceStrategyType,
			},
			RouteSelector: &metav1.LabelSelector{
				MatchLabels: routeSelectorlabels,
			},
		},
	}
	_, err := oc.AdminOperatorClient().OperatorV1().IngressControllers(ingressCtrl.Namespace).Create(context.Background(), ingressCtrl, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return ingressCtrl, waitForIngressControllerCondition(oc, timeout, types.NamespacedName{Namespace: ingressCtrl.Namespace, Name: ingressCtrl.Name}, ingressControllerLbAvailableConditions...)
}

func waitForIngressControllerCondition(oc *exutil.CLI, timeout time.Duration, name types.NamespacedName, conditions ...operatorv1.OperatorCondition) error {
	return wait.PollImmediate(3*time.Second, timeout, func() (bool, error) {
		ic, err := oc.AdminOperatorClient().OperatorV1().IngressControllers(name.Namespace).Get(context.Background(), name.Name, metav1.GetOptions{})
		if err != nil {
			e2e.Logf("failed to get ingresscontroller %s/%s: %v, retrying...", name.Namespace, name.Name, err)
			return false, nil
		}
		expected := operatorConditionMap(conditions...)
		current := operatorConditionMap(ic.Status.Conditions...)
		met := conditionsMatchExpected(expected, current)
		if !met {
			e2e.Logf("ingresscontroller %s/%s conditions not met; wanted %+v, got %+v, retrying...", name.Namespace, name.Name, expected, current)
		}
		return met, nil
	})
}

// Create https GET towards a given domain using a given IP on transport level. Returns the response or error.
func httpsGetWithCustomLookup(url string, ip string) (*http.Response, error) {

	// Create a custom resolver that overrides DNS lookups
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			// Perform custom DNS resolution here
			ip := ip
			dialer := net.Dialer{
				Timeout: time.Second * 10,
			}
			return dialer.DialContext(ctx, network, ip+":443")
		},
	}

	// Create a custom HTTP transport with the custom resolver
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		DialContext:           resolver.Dial,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// Create an HTTP client with the custom transport
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
	defer client.CloseIdleConnections()

	// Send an HTTP GET request
	resp, err := client.Get("https://" + url)
	if err != nil {
		return nil, err
	}
	return resp, err
}
