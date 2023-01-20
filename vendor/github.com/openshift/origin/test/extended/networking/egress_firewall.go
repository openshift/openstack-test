package networking

import (
	"context"
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/openshift/origin/test/extended/util/image"

	kapiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	frameworkpod "k8s.io/kubernetes/test/e2e/framework/pod"
	admissionapi "k8s.io/pod-security-admission/api"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
)

const (
	egressFWTestPod      = "egressfirewall"
	egressFWE2E          = "egress-firewall-e2e"
	noEgressFWE2E        = "no-egress-firewall-e2e"
	egressFWTestImage    = "quay.io/redhat-developer/nfs-server:1.1"
	oVNKManifest         = "ovnk-egressfirewall-test.yaml"
	openShiftSDNManifest = "sdn-egressnetworkpolicy-test.yaml"
)

var _ = g.Describe("[sig-network][Feature:EgressFirewall]", func() {

	egFwoc := exutil.NewCLIWithPodSecurityLevel(egressFWE2E, admissionapi.LevelPrivileged)
	egFwf := egFwoc.KubeFramework()

	// The OVNKubernetes subnet plugin supports EgressFirewall objects.
	InOVNKubernetesContext(
		func() {
			g.It("should ensure egressfirewall is created", func() {
				doEgressFwTest(egFwf, egFwoc, oVNKManifest)
			})
		},
	)
	// For Openshift SDN its supports EgressNetworkPolicy objects
	InOpenShiftSDNContext(
		func() {
			g.It("should ensure egressnetworkpolicy is created", func() {
				doEgressFwTest(egFwf, egFwoc, openShiftSDNManifest)
			})
		},
	)
	noegFwoc := exutil.NewCLIWithPodSecurityLevel(noEgressFWE2E, admissionapi.LevelBaseline)
	noegFwf := noegFwoc.KubeFramework()
	g.It("egressFirewall should have no impact outside its namespace", func() {
		g.By("creating test pod")
		pod := "dummy"
		o.Expect(createTestEgressFw(noegFwf, pod)).To(o.Succeed())
		// Skip EgressFw test if we cannot reach to external servers
		if !checkConnectivityToExternalHost(noegFwf, noegFwoc, pod) {
			e2e.Logf("Skip doing egress firewall")
			deleteTestEgressFw(noegFwf)
			return
		}
		g.By("sending traffic should all pass with no egress firewall impact")
		infra, err := noegFwoc.AdminConfigClient().ConfigV1().Infrastructures().Get(context.Background(), "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to get cluster-wide infrastructure")

		if platformSupportICMP(infra.Status.PlatformStatus.Type) {
			_, err := noegFwoc.Run("exec").Args(pod, "--", "ping", "-c", "1", "8.8.8.8").Output()
			expectNoError(err)

			_, err = noegFwoc.Run("exec").Args(pod, "--", "ping", "-c", "1", "1.1.1.1").Output()
			expectNoError(err)
		}
		_, err = noegFwoc.Run("exec").Args(pod, "--", "curl", "-q", "-s", "-I", "-m1", "https://docs.openshift.com").Output()
		expectNoError(err)

		_, err = noegFwoc.Run("exec").Args(pod, "--", "curl", "-q", "-s", "-I", "-m1", "http://www.google.com:80").Output()
		expectNoError(err)
		deleteTestEgressFw(noegFwf)
	})
})

func doEgressFwTest(f *e2e.Framework, oc *exutil.CLI, manifest string) error {
	g.By("creating test pod")
	o.Expect(createTestEgressFw(f, egressFWTestPod)).To(o.Succeed())

	// Skip EgressFw test if we cannot reach to external servers
	if !checkConnectivityToExternalHost(f, oc, egressFWTestPod) {
		e2e.Logf("Skip doing egress firewall")
		deleteTestEgressFw(f)
		return nil
	}
	g.By("creating an egressfirewall object")
	egFwYaml := exutil.FixturePath("testdata", "egress-firewall", manifest)

	g.By(fmt.Sprintf("calling oc create -f %s", egFwYaml))
	err := oc.AsAdmin().Run("create").Args("-f", egFwYaml).Execute()
	o.Expect(err).NotTo(o.HaveOccurred(), "created egress-firewall object")

	o.Expect(sendEgressFwTraffic(f, oc, egressFWTestPod)).To(o.Succeed())

	g.By("deleting test pod")
	deleteTestEgressFw(f)
	return err
}

func sendEgressFwTraffic(f *e2e.Framework, oc *exutil.CLI, pod string) error {
	infra, err := oc.AdminConfigClient().ConfigV1().Infrastructures().Get(context.Background(), "cluster", metav1.GetOptions{})
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to get cluster-wide infrastructure")

	if platformSupportICMP(infra.Status.PlatformStatus.Type) {
		// Test ICMP / Ping to Google’s DNS IP (8.8.8.8) should pass
		// because we have allow cidr rule for 8.8.8.8
		g.By("sending traffic that matches allow cidr rule")
		_, err := oc.Run("exec").Args(pod, "--", "ping", "-c", "1", "8.8.8.8").Output()
		expectNoError(err)

		// Test ICMP / Ping to Cloudfare DNS IP (1.1.1.1) should fail
		// because there is no allow cidr match for 1.1.1.1
		g.By("sending traffic that does not match allow cidr rule")
		_, err = oc.Run("exec").Args(pod, "--", "ping", "-c", "1", "1.1.1.1").Output()
		expectError(err)
	}
	// Test curl to docs.openshift.com should pass
	// because we have allow dns rule for docs.openshift.com
	g.By("sending traffic that matches allow dns rule")
	_, err = oc.Run("exec").Args(pod, "--", "curl", "-q", "-s", "-I", "-m1", "https://docs.openshift.com").Output()
	expectNoError(err)

	// Test curl to www.google.com:80 should fail
	// because we don't have allow dns rule for www.google.com:80
	g.By("sending traffic that does not match allow dns rule")
	_, err = oc.Run("exec").Args(pod, "--", "curl", "-q", "-s", "-I", "-m1", "http://www.google.com:80").Output()
	expectError(err)
	return nil
}

func checkConnectivityToExternalHost(f *e2e.Framework, oc *exutil.CLI, pod string) bool {
	g.By("executing a successful access to external internet")
	_, err := oc.Run("exec").Args(pod, "--", "curl", "-q", "-s", "-I", "-m1", "http://www.google.com:80").Output()
	if err != nil {
		e2e.Logf("Unable to connect/talk to the internet: %v", err)
		return false
	}
	return true
}

func createTestEgressFw(f *e2e.Framework, pod string) error {
	makeNamespaceScheduleToAllNodes(f)

	var nodes *kapiv1.Node
	var err error
	nodes, _, err = findAppropriateNodes(f, DIFFERENT_NODE)
	if err != nil {
		return err
	}
	err = launchTestEgressFwPod(f, nodes.Name, pod)
	expectNoError(err)

	_, err = waitForTestEgressFwPod(f, pod)
	expectNoError(err)
	return nil
}

func deleteTestEgressFw(f *e2e.Framework) error {
	var zero int64
	pod := egressFWTestPod
	err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(context.Background(), pod, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	return err
}

func launchTestEgressFwPod(f *e2e.Framework, nodeName string, podName string) error {
	contName := fmt.Sprintf("%s-container", podName)
	pod := &kapiv1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind: "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: kapiv1.PodSpec{
			Containers: []kapiv1.Container{
				{
					Name:    contName,
					Image:   image.LocationFor(egressFWTestImage),
					Command: []string{"sleep", "1000"},
				},
			},
			NodeName:      nodeName,
			RestartPolicy: kapiv1.RestartPolicyNever,
		},
	}
	_, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(context.Background(), pod, metav1.CreateOptions{})
	return err
}

func waitForTestEgressFwPod(f *e2e.Framework, podName string) (string, error) {
	podIP := ""
	err := frameworkpod.WaitForPodCondition(f.ClientSet, f.Namespace.Name, podName, "running", podStartTimeout, func(pod *kapiv1.Pod) (bool, error) {
		podIP = pod.Status.PodIP
		return (podIP != "" && pod.Status.Phase != kapiv1.PodPending), nil
	})
	return podIP, err
}

func platformSupportICMP(platformType configv1.PlatformType) bool {
	switch platformType {
	// Azure has secuirty rules to prevent icmp response from outside the cluster by default
	case configv1.AzurePlatformType:
		return false
	default:
		return true
	}
}
