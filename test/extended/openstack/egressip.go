package openstack

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	cloudnetwork "github.com/openshift/client-go/cloudnetwork/clientset/versioned"
	"github.com/openshift/openstack-test/test/extended/openstack/client"
	exutil "github.com/openshift/origin/test/extended/util"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	e2e "k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

const (
	egressIPConfigAnnotationKey = "cloud.network.openshift.io/egress-ipconfig"
	egressAssignableLabelKey    = "k8s.ovn.org/egress-assignable"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack][egressip] An egressIP", func() {
	var networkClient *gophercloud.ServiceClient
	var clientSet *kubernetes.Clientset
	var err error
	var workerNodeList *corev1.NodeList
	var cloudNetworkClientset cloudnetwork.Interface
	oc := exutil.NewCLI("openstack")

	g.BeforeEach(func(ctx g.SpecContext) {
		g.By("Loading the kubernetes clientset")
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("preparing the openstack client")
		networkClient, err = client.GetServiceClient(ctx, openstack.NewNetworkV2)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to build the OpenStack client")

		// TODO revert once https://issues.redhat.com/browse/OSASINFRA-3079 is resolved
		proxy, err := oc.AdminConfigClient().ConfigV1().Proxies().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		if os.Getenv("HTTP_PROXY") != "" || os.Getenv("HTTPS_PROXY") != "" || proxy.Status.HTTPProxy != "" {
			e2eskipper.Skipf("Test not applicable for proxy setup")
		}

		g.By("Getting the worker node list")
		workerNodeList, err = clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/worker,node-role.kubernetes.io/infra!=",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It("attached to a floating IP should be kept after EgressIP node failover with OVN-Kubernetes NetworkType", func(ctx g.SpecContext) {

		g.By("Getting the network type")
		networkType, err := getNetworkType(ctx, oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		if networkType != NetworkTypeOVNKubernetes {
			e2eskipper.Skipf("Test not applicable for '%s' NetworkType (only valid for '%s')", networkType, NetworkTypeOVNKubernetes)
		}

		singleStackIpv6, err := isSingleStackIpv6Cluster(ctx, oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		if singleStackIpv6 { //This test is covering and scenario that has no sense with ipv6 as there is no FIP/VIP association.
			e2eskipper.Skipf("Test not applicable for singleStack IPv6 environments")
		}

		// Skip based on number of required worker nodes
		minWorkerNum := 2
		g.By(fmt.Sprintf("Check the number of worker nodes (should be %d at least)", minWorkerNum))
		e2e.Logf("Detected number of workers: '%d'", len(workerNodeList.Items))
		if len(workerNodeList.Items) < minWorkerNum {
			e2eskipper.Skipf("Skipping test as there are '%d' worker node(s) and minimum required is '%d'", len(workerNodeList.Items), minWorkerNum)
		}

		e2e.Logf("Getting 2 workers for the EgressIP node failover checks, aka primary and secondary workers")
		primaryWorker := workerNodeList.Items[0]
		secondaryWorker := workerNodeList.Items[1]
		e2e.Logf("Selected primary EgressIP assignable worker: %s", primaryWorker.Name)
		e2e.Logf("Selected secondary EgressIP assignable worker: %s", secondaryWorker.Name)

		// Label the primary worker node as EgressIP assignable node
		// "dummy" is added as value for the label key just because RemoveLabelOffNode is not
		// able to remove the key if it doesn't have any value
		g.By(fmt.Sprintf("Labeling the primary node '%s' with '%s'", primaryWorker.Name, egressAssignableLabelKey))
		node.AddOrUpdateLabelOnNode(clientSet, primaryWorker.Name, egressAssignableLabelKey, "dummy")
		defer node.RemoveLabelOffNode(clientSet, primaryWorker.Name, egressAssignableLabelKey)

		g.By(fmt.Sprintf("Getting the EgressIP network from the '%s' annotation", egressIPConfigAnnotationKey))
		egressIPNetCidrStr, egressPortId, err := getEgressNetworkInfo(primaryWorker, "ipv4")
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(egressIPNetCidrStr).NotTo(o.BeEmpty(), "Could not get the EgressIP network from the '%s' annotation", egressIPConfigAnnotationKey)
		e2e.Logf("Found the EgressIP network: %s", egressIPNetCidrStr)
		o.Expect(egressPortId).NotTo(o.BeEmpty(), "Could not get the Egress openstack portId from the '%s' annotation", egressPortId)
		e2e.Logf("Found the Egress PortID: %s", egressPortId)

		g.By("Finding the openstack network ID from the egressPortId defined in openshift node annotation")
		machineNetworkID, err := getNetworkIdFromPortId(ctx, networkClient, egressPortId)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(machineNetworkID).NotTo(o.BeEmpty(), "Could not get the EgressIP network ID in openstack for '%s' subnet CIDR", egressIPNetCidrStr)
		e2e.Logf("Found the EgressIP network ID '%s' in Openstack for the EgressIP CIDR '%s'", machineNetworkID, egressIPNetCidrStr)

		egressIPAddrStr, err := getNotInUseEgressIP(ctx, networkClient, egressIPNetCidrStr, machineNetworkID)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(egressIPAddrStr).NotTo(o.BeEmpty(), "Couldn't find a free IP address in '%s' network in Openstack", egressIPNetCidrStr)
		e2e.Logf("Found '%s' free IP address in the EgressIP network in Openstack", egressIPAddrStr)

		g.By("Creating a temp directory")
		egressIPTempDir, err := os.MkdirTemp("", "egressip-e2e")
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("Created '%s' temporary directory", egressIPTempDir)
		defer os.RemoveAll(egressIPTempDir)

		g.By("Create egressIP resource in openshift")
		egressIPname := "egress-ip"
		err = createEgressIpResource(oc, egressIPname, egressIPAddrStr, "app: egress")
		o.Expect(err).NotTo(o.HaveOccurred())
		defer oc.AsAdmin().Run("delete").Args("egressip", egressIPname).Execute()

		g.By("Waiting until CloudPrivateIPConfig is created and assigned to the primary worker node")
		cloudNetworkClientset, err = cloudnetwork.NewForConfig(oc.AdminConfig())
		o.Expect(err).NotTo(o.HaveOccurred())
		waitOk, err := waitCloudPrivateIPConfigAssignedNode(ctx, cloudNetworkClientset, egressIPAddrStr, primaryWorker.Name)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(waitOk).To(o.BeTrue(), "Not found the expected assigned node '%s' in '%s' CloudPrivateIPConfig", primaryWorker.Name, egressIPAddrStr)
		e2e.Logf("Found the expected assigned node '%s' in '%s' CloudPrivateIPConfig", primaryWorker.Name, egressIPAddrStr)

		// Label the secondary worker node as EgressIP assignable node
		g.By(fmt.Sprintf("Labeling the secondary node '%s' with '%s'", secondaryWorker.Name, egressAssignableLabelKey))
		node.AddOrUpdateLabelOnNode(clientSet, secondaryWorker.Name, egressAssignableLabelKey, "dummy")
		defer node.RemoveLabelOffNode(clientSet, secondaryWorker.Name, egressAssignableLabelKey)

		// Check the assigned node in CloudPrivateIPConfig is kept with the primary EgressIP worker node
		g.By("Checking the CloudPrivateIPConfig object and the assigned node after adding a second EgressIP assignable node")
		cpicAssignedNode, err := getCpipAssignedNode(ctx, cloudNetworkClientset, egressIPAddrStr)
		o.Expect(err).NotTo(o.HaveOccurred(), "Could not find the assigned node in '%s' CloudPrivateIPConfig", egressIPAddrStr)
		e2e.Logf("'%s' CloudPrivateIPConfig assigned node ('%s') is kept after adding a second assignable node", egressIPAddrStr, cpicAssignedNode)
		o.Expect(cpicAssignedNode).To(o.Equal(primaryWorker.Name), "'%s' egressIP has been deassigned to the '%s' node", egressIPAddrStr, primaryWorker.Name)

		g.By("Checking a port has been created in Openstack for the EgressIP")
		ports, err := getPortsByIP(ctx, networkClient, egressIPAddrStr, machineNetworkID)
		o.Expect(err).NotTo(o.HaveOccurred(), "Could not find the Openstack port with IP address '%s'", egressIPAddrStr)
		o.Expect(ports).To(o.HaveLen(1), "Unexpected number of Openstack ports (%d) found with IP address '%s'", len(ports), egressIPAddrStr)
		egressIPPort := ports[0]
		e2e.Logf("Found '%s' Openstack port with IP address '%s'", egressIPPort.Name, egressIPAddrStr)

		g.By("Checking that the allowed_addresses_pairs are properly updated for the workers in the Openstack before failover")
		checkAllowedAddressesPairs(ctx, networkClient, primaryWorker, secondaryWorker, egressIPAddrStr, machineNetworkID)

		g.By("Fetching an external network for the cluster")
		cloudProviderConfig, err := getConfig(ctx,
			oc.AdminKubeClient(),
			"openshift-cloud-controller-manager",
			"cloud-conf",
			"cloud.conf")
		o.Expect(err).NotTo(o.HaveOccurred())
		var fip *floatingips.FloatingIP
		externalNetworkId, err := GetFloatingNetworkID(ctx, networkClient, cloudProviderConfig)
		o.Expect(err).NotTo(o.HaveOccurred())
		g.By("Creating a FIP in Openstack")
		fip, err = floatingips.Create(ctx, networkClient, floatingips.CreateOpts{FloatingNetworkID: externalNetworkId}).Extract()
		o.Expect(err).NotTo(o.HaveOccurred(), "Error creating FIP using discovered floatingNetwork ID '%s'", externalNetworkId)
		e2e.Logf("The FIP '%s' has been created in Openstack", fip.FloatingIP)
		defer floatingips.Delete(ctx, networkClient, fip.ID)

		g.By(fmt.Sprintf("Attaching the FIP %s to the EgressIP port '%s'", fip.FloatingIP, egressIPPort.Name))
		fip, err = floatingips.Update(ctx, networkClient, fip.ID, floatingips.UpdateOpts{PortID: &egressIPPort.ID}).Extract()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(fip.FixedIP).To(o.Equal(egressIPAddrStr), "FIP '%s' Fixed IP Address (%s) is not the expected one (%s)", fip.FloatingIP, fip.FixedIP, egressIPAddrStr)
		e2e.Logf("Fixed IP (%s) attached to FIP '%s'", egressIPAddrStr, fip.FloatingIP)

		g.By("Removing the egress-assignable label from the primary worker node (simulates a node failover)")
		node.RemoveLabelOffNode(clientSet, primaryWorker.Name, egressAssignableLabelKey)

		g.By("Waiting until CloudPrivateIPConfig is updated with the secondary worker as assigned node")
		waitOk, err = waitCloudPrivateIPConfigAssignedNode(ctx, cloudNetworkClientset, egressIPAddrStr, secondaryWorker.Name)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(waitOk).To(o.BeTrue(), "Not found the expected assigned node '%s' in '%s' CloudPrivateIPConfig", secondaryWorker.Name, egressIPAddrStr)
		e2e.Logf("Found the expected assigned node '%s' in '%s' CloudPrivateIPConfig", secondaryWorker.Name, egressIPAddrStr)

		g.By("Checking the Openstack port is kept after the EgressIP failover")
		ports, err = getPortsByIP(ctx, networkClient, egressIPAddrStr, machineNetworkID)
		o.Expect(err).NotTo(o.HaveOccurred(), "Could not find the Openstack port with IP address '%s'", egressIPAddrStr)
		o.Expect(ports).To(o.HaveLen(1), "Unexpected number of Openstack ports (%d) found with IP address '%s'", len(ports), egressIPAddrStr)
		o.Expect(ports[0].ID).To(o.Equal(egressIPPort.ID), "Found different Openstack port ID for the EgressIP after the EgressIP failover")
		e2e.Logf("The Openstack port '%s' has been kept after the EgressIP failover", egressIPPort.Name)

		g.By("Checking that the allowed_addresses_pairs are properly updated for the workers in the Openstack after failover")
		checkAllowedAddressesPairs(ctx, networkClient, secondaryWorker, primaryWorker, egressIPAddrStr, machineNetworkID)

		g.By("Checking the FIP Fixed IP address is kept")
		fip, err = floatingips.Get(ctx, networkClient, fip.ID).Extract()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(fip.FixedIP).To(o.Equal(egressIPAddrStr), "FIP '%s' Fixed IP Address (%s) is not the expected one (%s)", fip.FloatingIP, fip.FixedIP, egressIPAddrStr)
		e2e.Logf("Found the expected Fixed IP (%s) for the FIP '%s'", egressIPAddrStr, fip.FloatingIP)

	})

	// https://issues.redhat.com/browse/OCPBUGS-27222
	g.It("with IPv6 format should be created on dualstack or ssipv6 cluster with OVN-Kubernetes NetworkType and dhcpv6-stateful mode", func(ctx g.SpecContext) {

		networks, err := oc.AdminConfigClient().ConfigV1().Networks().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		dualstack, err := isDualStackCluster(networks.Status.ClusterNetwork)
		o.Expect(err).NotTo(o.HaveOccurred())
		ssipv6, err := isSingleStackIpv6Cluster(ctx, oc)
		o.Expect(err).NotTo(o.HaveOccurred())

		if !(dualstack || ssipv6) {
			e2eskipper.Skipf("Test only applicable for dualstack or SingleStack IPv6 clusters")
		}

		g.By("Getting the network type")
		networkType, err := getNetworkType(ctx, oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		if networkType != NetworkTypeOVNKubernetes {
			e2eskipper.Skipf("Test not applicable for '%s' NetworkType (only valid for '%s')", networkType, NetworkTypeOVNKubernetes)
		}

		e2e.Logf("Getting the worker for the EgressIP")
		worker := workerNodeList.Items[0]

		g.By(fmt.Sprintf("Getting the EgressIP network from the '%s' annotation", egressIPConfigAnnotationKey))
		egressIPNetCidrStr, egressPortId, err := getEgressNetworkInfo(worker, "ipv6")
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(egressIPNetCidrStr).NotTo(o.BeEmpty(), "Could not get the EgressIP network from the '%s' annotation", egressIPConfigAnnotationKey)
		e2e.Logf("Found the EgressIP network: %s", egressIPNetCidrStr)
		o.Expect(egressPortId).NotTo(o.BeEmpty(), "Could not get the Egress openstack portId from the '%s' annotation", egressPortId)
		e2e.Logf("Found the Egress PortID: %s", egressPortId)

		g.By("Finding the openstack network ID from the egressPortId defined in openshift node annotation")
		machineNetworkID, err := getNetworkIdFromPortId(ctx, networkClient, egressPortId)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(machineNetworkID).NotTo(o.BeEmpty(), "Could not get the EgressIP network ID in openstack for '%s' subnet CIDR", egressIPNetCidrStr)
		e2e.Logf("Found the EgressIP network ID '%s' in Openstack for the EgressIP CIDR '%s'", machineNetworkID, egressIPNetCidrStr)

		g.By("Discovering the ipv6 mode configured in the subnet")
		ipv6mode, err := getipv6ModeFromSubnetCidr(ctx, networkClient, egressIPNetCidrStr, machineNetworkID)
		o.Expect(err).NotTo(o.HaveOccurred())
		if ipv6mode != "dhcpv6-stateful" {
			e2eskipper.Skipf("Test not applicable for '%s' ipv6mode (only valid for '%s')", ipv6mode, "dhcpv6-stateful")
		}

		// Label the worker node as EgressIP assignable node
		g.By(fmt.Sprintf("Labeling the primary node '%s' with '%s'", worker.Name, egressAssignableLabelKey))
		node.AddOrUpdateLabelOnNode(clientSet, worker.Name, egressAssignableLabelKey, "dummy")
		defer node.RemoveLabelOffNode(clientSet, worker.Name, egressAssignableLabelKey)

		g.By("Looking for a free IP in the subnet to use for the egressIP object in openshift")
		egressIPAddrStr, err := getNotInUseEgressIP(ctx, networkClient, egressIPNetCidrStr, machineNetworkID)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(egressIPAddrStr).NotTo(o.BeEmpty(), "Couldn't find a free IP address in '%s' network in Openstack", egressIPNetCidrStr)
		o.Expect(isIpv6(egressIPAddrStr)).To(o.BeTrue(), "egressIP should be IPv6 but it's %q", egressIPAddrStr)
		e2e.Logf("Found '%s' free IP address in the EgressIP network in Openstack", egressIPAddrStr)

		g.By("Create egressIP resource in openshift")
		egressIPname := "egress-ipv6"
		err = createEgressIpResource(oc, egressIPname, egressIPAddrStr, "app: egress")
		o.Expect(err).NotTo(o.HaveOccurred())
		defer oc.AsAdmin().Run("delete").Args("egressip", egressIPname).Execute()

		g.By("Waiting until CloudPrivateIPConfig is created and assigned to the primary worker node")
		cloudNetworkClientset, err = cloudnetwork.NewForConfig(oc.AdminConfig())
		o.Expect(err).NotTo(o.HaveOccurred())
		egressIPAddr, err := netip.ParseAddr(egressIPAddrStr)
		o.Expect(err).NotTo(o.HaveOccurred())
		waitOk, err := waitCloudPrivateIPConfigAssignedNode(ctx, cloudNetworkClientset, strings.ReplaceAll(egressIPAddr.StringExpanded(), ":", "."), worker.Name)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(waitOk).To(o.BeTrue(), "Not found the expected assigned node '%s' in '%s' CloudPrivateIPConfig", worker.Name, egressIPAddrStr)
		e2e.Logf("Found the expected assigned node '%s' in '%s' CloudPrivateIPConfig", worker.Name, egressIPAddrStr)

		g.By("Checking that the port exists from openstack perspective")
		egressNetInUseIPs, err := getInUseIPs(ctx, networkClient, machineNetworkID)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(egressNetInUseIPs).To(o.ContainElement(egressIPAddrStr))

		g.By("Checking that the allowed_addresses_pairs are properly updated for the worker in the Openstack")
		checkAllowedAddressesPairs(ctx, networkClient, worker, corev1.Node{}, egressIPAddrStr, machineNetworkID)
	})
})

// getNotInUseEgressIP returns a not in use IP address from the EgressIP network CIDR
func getNotInUseEgressIP(ctx context.Context, client *gophercloud.ServiceClient, egressCidr string, egressIPNetID string) (string, error) {

	// Obtain the IPs in use in the EgressIP network in Openstack
	egressNetInUseIPs, err := getInUseIPs(ctx, client, egressIPNetID)
	if err != nil {
		return "", err
	}
	e2e.Logf("In use IP address list in the EgressIP network: %v", egressNetInUseIPs)

	// Obtain a not in use IP address in the EgressIP network in Openstack
	freeIP, err := getFreeIP(egressCidr, egressNetInUseIPs)
	if err != nil {
		return "", err
	}
	return freeIP, nil
}

// getEgressNetworkInfo returns the IP address CIDR and openstack portId from the node egress-ipconfig annotation
func getEgressNetworkInfo(node corev1.Node, ipVersion string) (string, string, error) {
	type ifAddr struct {
		IPv4 string `json:"ipv4,omitempty"`
		IPv6 string `json:"ipv6,omitempty"`
	}
	type capacity struct {
		IPv4 int `json:"ipv4,omitempty"`
		IPv6 int `json:"ipv6,omitempty"`
		IP   int `json:"ip,omitempty"`
	}
	type NodeEgressIPConfiguration struct {
		Interface string   `json:"interface"`
		IFAddr    ifAddr   `json:"ifaddr"`
		Capacity  capacity `json:"capacity"`
	}

	annotation, ok := node.Annotations[egressIPConfigAnnotationKey]
	if !ok {
		e2e.Logf("Annotation '%s' not found in '%s' node", egressIPConfigAnnotationKey, node.Name)
		return "", "", nil
	}
	e2e.Logf("Found '%s' annotation in '%s': %s", egressIPConfigAnnotationKey, node.Name, annotation)
	var nodeEgressIPConfigs []*NodeEgressIPConfiguration
	err := json.Unmarshal([]byte(annotation), &nodeEgressIPConfigs)
	if err != nil {
		return "", "", err
	}
	egressIPNetStr := ""
	if ipVersion == "ipv6" {
		egressIPNetStr = nodeEgressIPConfigs[0].IFAddr.IPv6
		if egressIPNetStr == "" {
			e2e.Logf("Empty ifaddr.ipv6 in the '%s' annotation", egressIPConfigAnnotationKey)
			return "", "", nil
		}
	} else if ipVersion == "ipv4" {
		egressIPNetStr = nodeEgressIPConfigs[0].IFAddr.IPv4
		if egressIPNetStr == "" {
			e2e.Logf("Empty ifaddr.ipv4 in the '%s' annotation", egressIPConfigAnnotationKey)
			return "", "", nil
		}
	} else {
		return "", "", fmt.Errorf("Unknown ipVersion. Only ipv4 and ipv6 supported")
	}

	return egressIPNetStr, nodeEgressIPConfigs[0].Interface, nil
}

// getNetworkIdFromPortId returns the Openstack network ID for a given Openstack port
func getNetworkIdFromPortId(ctx context.Context, client *gophercloud.ServiceClient, portId string) (string, error) {
	port, err := ports.Get(ctx, client, portId).Extract()
	if err != nil {
		return "", fmt.Errorf("failed to get port")
	}
	return port.NetworkID, nil
}

func getipv6ModeFromSubnetCidr(ctx context.Context, client *gophercloud.ServiceClient, subnetCidr string, networkId string) (string, error) {
	listOpts := subnets.ListOpts{
		CIDR:      subnetCidr,
		NetworkID: networkId,
	}
	allPages, err := subnets.List(client, listOpts).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get subnets")
	}
	allSubnets, err := subnets.ExtractSubnets(allPages)
	if err != nil {
		return "", fmt.Errorf("failed to extract subnets")
	}
	if len(allSubnets) != 1 {
		return "", fmt.Errorf("unexpected number of subnets found  with '%s' CIDR: %d subnets", subnetCidr, len(allSubnets))
	}
	return allSubnets[0].IPv6AddressMode, nil
}

// getInUseIPs returns the in use IPs in a given network ID in Openstack
func getInUseIPs(ctx context.Context, client *gophercloud.ServiceClient, networkId string) ([]string, error) {
	var inUseIPs []string
	portListOpts := ports.ListOpts{
		NetworkID: networkId,
	}
	allPages, err := ports.List(client, portListOpts).AllPages(ctx)
	if err != nil {
		e2e.Logf("Failed to get ports")
		return nil, err
	}
	allPorts, err := ports.ExtractPorts(allPages)
	if err != nil {
		e2e.Logf("Failed to extract ports")
		return nil, err
	}
	for _, port := range allPorts {
		for _, fixedIpPort := range port.FixedIPs {
			inUseIPs = append(inUseIPs, fixedIpPort.IPAddress)
		}
	}
	return inUseIPs, nil
}

// getFreeIP returns an IP address that is not in use in Openstack for a given network CIDR
func getFreeIP(networkCIDR string, reservedIPs []string) (string, error) {
	_, ipNetwork, err := net.ParseCIDR(networkCIDR)
	if err != nil {
		return "", err
	}
	ipAddr := ipNetwork.IP
	ipAddrStr := ""
	for ; ipNetwork.Contains(ipAddr); ipAddr = incIP(ipAddr) {
		// Skip network reserved IPs (network and broadcast)
		ipAddrStr = ipAddr.String()
		var excludeIPs = []string{ipNetwork.IP.String(), getBroadCastIP(*ipNetwork).String()}
		if contains(excludeIPs, ipAddrStr) {
			continue
		}
		if !contains(reservedIPs, ipAddrStr) {
			return ipAddrStr, nil
		}
	}
	return "", nil
}

// getBroadCastIP returns the last IP of a subnet
func getBroadCastIP(subnet net.IPNet) net.IP {
	var end net.IP
	for i := 0; i < len(subnet.IP); i++ {
		end = append(end, subnet.IP[i]|^subnet.Mask[i])
	}
	return end
}

// https://andreaskaris.github.io/blog/networking/golang-ip-conversion/
func incIP(ip net.IP) net.IP {
	// allocate a new IP
	newIp := make(net.IP, len(ip))
	copy(newIp, ip)

	byteIp := []byte(newIp)
	l := len(byteIp)
	var i int
	for k := range byteIp {
		// start with the rightmost index first
		// increment it
		// if the index is < 256, then no overflow happened and we increment and break
		// else, continue to the next field in the byte
		i = l - 1 - k
		if byteIp[i] < 0xff {
			byteIp[i]++
			break
		} else {
			byteIp[i] = 0
		}
	}
	return net.IP(byteIp)
}

// getCpipAssignedNode returns the assigned node from a given CloudPrivateIPConfig
func getCpipAssignedNode(ctx context.Context, cloudNetClientset cloudnetwork.Interface, egressIPAddr string) (string, error) {
	privIPconfig, err := cloudNetClientset.CloudV1().CloudPrivateIPConfigs().Get(ctx, egressIPAddr, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return privIPconfig.Spec.Node, nil
}

func getPortsByIP(ctx context.Context, client *gophercloud.ServiceClient, ipAddr string, networkID string) ([]ports.Port, error) {
	portListOpts := ports.ListOpts{
		FixedIPs:  []ports.FixedIPOpts{{IPAddress: ipAddr}},
		NetworkID: networkID,
	}
	allPages, err := ports.List(client, portListOpts).AllPages(ctx)
	if err != nil {
		e2e.Logf("Failed to get ports")
		return nil, err
	}
	allPorts, err := ports.ExtractPorts(allPages)
	if err != nil {
		e2e.Logf("Failed to extract ports")
		return nil, err
	}
	return allPorts, nil
}

// Active Wait for CloudPrivateIPConfig resource in OCP to be assigned to the expected node
func waitCloudPrivateIPConfigAssignedNode(ctx context.Context, cloudNetClientset cloudnetwork.Interface, egressIP string, node string) (bool, error) {
	o.Eventually(func() bool {
		privIPconfig, err := cloudNetClientset.CloudV1().CloudPrivateIPConfigs().Get(ctx, egressIP, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				e2e.Logf("'%s' CloudPrivateIPConfig not found", egressIP)
			} else {
				e2e.Logf("Error ocurred: %v, trying next iteration", err)
			}
			return false
		}
		for _, condition := range privIPconfig.Status.Conditions {
			if condition.Type == "Assigned" && condition.Status == metav1.ConditionTrue {
				if privIPconfig.Spec.Node == node {
					return true
				} else {
					e2e.Logf("'%s' CloudPrivateIPConfig assigned to the node '%s'", egressIP, privIPconfig.Spec.Node)
					return false
				}
			} else {
				e2e.Logf("'%s' CloudPrivateIPConfig not assigned", egressIP)
				return false
			}
		}
		return false
	}, "300s", "1s").Should(o.BeTrue(), "Timed out checking the assigned node '%s' in '%s' CloudPrivateIPConfig.", node, egressIP)
	return true, nil
}

// Returns the list of IPs present on the openstack allowed_address_pairs attribute in the node main port
func getAllowedIPsFromNode(ctx context.Context, client *gophercloud.ServiceClient, node corev1.Node, machineNetwork string) ([]string, error) {
	result := []string{}
	ip := strings.Split(node.GetAnnotations()["alpha.kubernetes.io/provided-node-ip"], ",")[0]
	nodePorts, err := getPortsByIP(ctx, client, ip, machineNetwork)
	if err != nil {
		return nil, err
	}
	if len(nodePorts) != 1 {
		return nil, fmt.Errorf("unexpected number of openstack ports (%d) for IP %s", len(nodePorts), ip)
	}
	for _, addressPair := range nodePorts[0].AllowedAddressPairs {
		result = append(result, addressPair.IPAddress)
	}
	return result, nil
}

// Checking the egressIP is added to allowed_address_pairs in the expected node and it is not present in the other
func checkAllowedAddressesPairs(ctx context.Context, client *gophercloud.ServiceClient, nodeHoldingEgressIp corev1.Node, nodeNotHoldingEgressIp corev1.Node, egressIp string, networkID string) {

	o.Eventually(func() bool {
		allowedIpList, err := getAllowedIPsFromNode(ctx, client, nodeHoldingEgressIp, networkID)
		if err != nil {
			e2e.Logf("error obtaining allowedIpList: %q", err)
			return false
		} else if !contains(allowedIpList, egressIp) {
			e2e.Logf("egressIP %s still not found on obtained allowedIpList (%s) for node %s", egressIp, allowedIpList, nodeHoldingEgressIp.Name)
			return false
		}
		return true
	}, "10s", "1s").Should(o.BeTrue(), "Timed out checking allowed address pairs for node %s", nodeHoldingEgressIp.Name)
	e2e.Logf("egressIp %s correctly included on the node allowed-address-pairs for %s", egressIp, nodeHoldingEgressIp.Name)

	if nodeNotHoldingEgressIp.Name != "" {
		o.Eventually(func() bool {
			allowedIpList, err := getAllowedIPsFromNode(ctx, client, nodeNotHoldingEgressIp, networkID)
			if err != nil {
				e2e.Logf("error obtaining allowedIpList")
				return false
			} else if contains(allowedIpList, egressIp) {
				e2e.Logf("egressIP %s still found on obtained allowedIpList (%s) for node %s", egressIp, allowedIpList, nodeNotHoldingEgressIp.Name)
				return false
			}
			return true
		}, "10s", "1s").Should(o.BeTrue(), "Timed out checking allowed address pairs for node %s", nodeNotHoldingEgressIp.Name)
		e2e.Logf("egressIp %s correctly not included on the node allowed-address-pairs for %s", egressIp, nodeNotHoldingEgressIp.Name)
	} else {
		e2e.Logf("Skipping check for worker not holding the egressIP")
	}
}

func createEgressIpResource(oc *exutil.CLI, egressIPname string, egressIPAddrStr string, labels string) error {

	g.By("Creating a temp directory")
	egressIPTempDir, err := os.MkdirTemp("", "egressip-e2e")
	if err != nil {
		return err
	}
	e2e.Logf("Created '%s' temporary directory", egressIPTempDir)
	defer os.RemoveAll(egressIPTempDir)

	g.By("Creating an EgressIP yaml file")
	var egressIPYamlFileName = "egressip.yaml"
	var egressIPYamlFilePath = egressIPTempDir + "/" + egressIPYamlFileName
	var egressIPYamlTemplate = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: %s
spec:
  egressIPs:
  - %s
  namespaceSelector:
    matchLabels:
      %s`

	egressIPYaml := fmt.Sprintf(egressIPYamlTemplate, egressIPname, egressIPAddrStr, labels)
	e2e.Logf("egressIPYaml: %s", egressIPYaml)

	err = os.WriteFile(egressIPYamlFilePath, []byte(egressIPYaml), 0644)
	if err != nil {
		return err
	}

	g.By(fmt.Sprintf("Creating an EgressIP object from '%s'", egressIPYamlFilePath))
	err = oc.AsAdmin().Run("create").Args("-f", egressIPYamlFilePath).Execute()
	if err != nil {
		return err
	}
	return nil
}
