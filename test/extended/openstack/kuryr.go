package openstack

import (
	"context"

	"github.com/gophercloud/gophercloud"
	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack][Kuryr] Kuryr", func() {
	var networkClient *gophercloud.ServiceClient
	var dc dynamic.Interface
	var clientSet *kubernetes.Clientset

	g.BeforeEach(func() {
		g.By("preparing openshift dynamic client")
		cfg, err := e2e.LoadConfig()
		o.Expect(err).NotTo(o.HaveOccurred())
		dc, err = dynamic.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		c, err := configv1client.NewForConfig(cfg)
		o.Expect(err).NotTo(o.HaveOccurred())
		network, err := c.ConfigV1().Networks().Get(context.Background(), "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		if network.Status.NetworkType != "Kuryr" {
			e2eskipper.Skipf("Test can only be run with Kuryr SDN")
		}

		g.By("preparing openstack client")
		networkClient, err = client(serviceNetwork)
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It("should create a subnet for a namespace only when a pod without hostNetwork is created in the namespace", func() {
		g.By("Creating a Namespace and pods")
		{
			ns := CreateNamespace(clientSet, "kuryr-hostnetwork")
			defer DeleteNamespace(clientSet, ns)
			g.By("Creating a Pod with hostNetwork=true")
			command := []string{"/bin/sleep", "120"}
			_, err := CreatePod(clientSet, ns.Name, "kuryr-hostnetwork", true, command)
			o.Expect(err).NotTo(o.HaveOccurred())
			subnetID, err := GetSubnetIDfromKuryrNetwork(clientSet, ns.Name)
			if !apierrors.IsNotFound(err) {
				e2e.Failf("Error has occured %v", err)
			}
			o.Expect(subnetID).To(o.BeEmpty(), "A subnet should not be created when a pod with Hostnetwork is created")
			g.By("Creating a Pod with hostNetwork=false")
			_, err = CreatePod(clientSet, ns.Name, "kuryr-nohostnetwork", false, command)
			o.Expect(err).NotTo(o.HaveOccurred())
			subnetID, err = GetSubnetIDfromKuryrNetwork(clientSet, ns.Name)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(subnetID).ToNot(o.BeEmpty(), "A subnet should be created when a non host network pod is created")
		}
	})
})
