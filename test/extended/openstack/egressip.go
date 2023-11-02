package openstack

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	cloudnetwork "github.com/openshift/client-go/cloudnetwork/clientset/versioned"
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
	var infraID string
	var workerNodeList *corev1.NodeList
	var cloudNetworkClientset cloudnetwork.Interface
	oc := exutil.NewCLI("openstack")

	var ctx context.Context

	g.BeforeEach(func() {
		ctx = context.Background()

		g.By("Loading the kubernetes clientset")
		clientSet, err = e2e.LoadClientset()
		o.Expect(err).NotTo(o.HaveOccurred())

		networkClient, err = client(serviceNetwork)
		o.Expect(err).NotTo(o.HaveOccurred(), "Error creating an openstack network client")

		// TODO revert once https://issues.redhat.com/browse/OSASINFRA-3079 is resolved
		proxy, err := oc.AdminConfigClient().ConfigV1().Proxies().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		if os.Getenv("HTTP_PROXY") != "" || os.Getenv("HTTPS_PROXY") != "" || proxy.Status.HTTPProxy != "" {
			e2eskipper.Skipf("Test not applicable for proxy setup")
		}

		infrastructure, err := oc.AdminConfigClient().ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		infraID = infrastructure.Status.InfrastructureName

		g.By("Getting the worker node list")
		workerNodeList, err = clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/worker",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It("attached to a floating IP should be kept after EgressIP node failover with OVN-Kubernetes NetworkType", func() {

		g.By("Getting the network type")
		networkType, err := getNetworkType(ctx, oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		if networkType != NetworkTypeOVNKubernetes {
			e2eskipper.Skipf("Test not applicable for '%s' NetworkType (only valid for '%s')", networkType, NetworkTypeOVNKubernetes)
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
		egressIPNetCidrStr, err := getEgressIPNetwork(primaryWorker)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(egressIPNetCidrStr).NotTo(o.BeEmpty(), "Could not get the EgressIP network from the '%s' annotation", egressIPConfigAnnotationKey)
		e2e.Logf("Found the EgressIP network: %s", egressIPNetCidrStr)

		g.By("Obtaining a not in use IP address from the EgressIP network (machineNetwork cidr)")
		machineNetworkID, err := getNetworkIdFromSubnetCidr(networkClient, egressIPNetCidrStr, infraID)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(machineNetworkID).NotTo(o.BeEmpty(), "Could not get the EgressIP network ID in openstack for '%s' subnet CIDR", egressIPNetCidrStr)
		e2e.Logf("Found the EgressIP network ID '%s' in Openstack for the EgressIP CIDR '%s'", machineNetworkID, egressIPNetCidrStr)

		egressIPAddrStr, err := getNotInUseEgressIP(networkClient, egressIPNetCidrStr, machineNetworkID)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(egressIPAddrStr).NotTo(o.BeEmpty(), "Couldn't find a free IP address in '%s' network in Openstack", egressIPNetCidrStr)
		e2e.Logf("Found '%s' free IP address in the EgressIP network in Openstack", egressIPAddrStr)

		g.By("Creating a temp directory")
		egressIPTempDir, err := os.MkdirTemp("", "egressip-e2e")
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("Created '%s' temporary directory", egressIPTempDir)
		defer os.RemoveAll(egressIPTempDir)

		g.By("Creating an EgressIP yaml file")
		var egressIPname = "egress-ip"
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

		egressIPYaml := fmt.Sprintf(egressIPYamlTemplate, egressIPname, egressIPAddrStr, "app: egress")
		e2e.Logf("egressIPYaml: %s", egressIPYaml)

		err = os.WriteFile(egressIPYamlFilePath, []byte(egressIPYaml), 0644)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("Creating an EgressIP object from '%s'", egressIPYamlFilePath))
		err = oc.AsAdmin().Run("create").Args("-f", egressIPYamlFilePath).Execute()
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
		ports, err := getPortsByIP(networkClient, egressIPAddrStr, machineNetworkID)
		o.Expect(err).NotTo(o.HaveOccurred(), "Could not find the Openstack port with IP address '%s'", egressIPAddrStr)
		o.Expect(ports).To(o.HaveLen(1), "Unexpected number of Openstack ports (%d) found with IP address '%s'", len(ports), egressIPAddrStr)
		egressIPPort := ports[0]
		e2e.Logf("Found '%s' Openstack port with IP address '%s'", egressIPPort.Name, egressIPAddrStr)

		g.By("Checking that the allowed_addresses_pairs are properly updated for the workers in the Openstack before failover")
		checkAllowedAddressesPairs(networkClient, primaryWorker, secondaryWorker, egressIPAddrStr, machineNetworkID)

		g.By("Creating a FIP in Openstack")
		var fip *floatingips.FloatingIP
		externalNetworkId, err := GetFloatingNetworkID(networkClient)
		o.Expect(err).NotTo(o.HaveOccurred())
		fip, err = floatingips.Create(networkClient, floatingips.CreateOpts{FloatingNetworkID: externalNetworkId}).Extract()
		o.Expect(err).NotTo(o.HaveOccurred(), "Error creating FIP using discovered floatingNetwork ID '%s'", externalNetworkId)
		e2e.Logf("The FIP '%s' has been created in Openstack", fip.FloatingIP)
		defer floatingips.Delete(networkClient, fip.ID)

		g.By(fmt.Sprintf("Attaching the FIP %s to the EgressIP port '%s'", fip.FloatingIP, egressIPPort.Name))
		fip, err = floatingips.Update(networkClient, fip.ID, floatingips.UpdateOpts{PortID: &egressIPPort.ID}).Extract()
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
		ports, err = getPortsByIP(networkClient, egressIPAddrStr, machineNetworkID)
		o.Expect(err).NotTo(o.HaveOccurred(), "Could not find the Openstack port with IP address '%s'", egressIPAddrStr)
		o.Expect(ports).To(o.HaveLen(1), "Unexpected number of Openstack ports (%d) found with IP address '%s'", len(ports), egressIPAddrStr)
		o.Expect(ports[0].ID).To(o.Equal(egressIPPort.ID), "Found different Openstack port ID for the EgressIP after the EgressIP failover")
		e2e.Logf("The Openstack port '%s' has been kept after the EgressIP failover", egressIPPort.Name)

		g.By("Checking that the allowed_addresses_pairs are properly updated for the workers in the Openstack after failover")
		checkAllowedAddressesPairs(networkClient, secondaryWorker, primaryWorker, egressIPAddrStr, machineNetworkID)

		g.By("Checking the FIP Fixed IP address is kept")
		fip, err = floatingips.Get(networkClient, fip.ID).Extract()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(fip.FixedIP).To(o.Equal(egressIPAddrStr), "FIP '%s' Fixed IP Address (%s) is not the expected one (%s)", fip.FloatingIP, fip.FixedIP, egressIPAddrStr)
		e2e.Logf("Found the expected Fixed IP (%s) for the FIP '%s'", egressIPAddrStr, fip.FloatingIP)

	})
})

// getNotInUseEgressIP returns a not in use IP address from the EgressIP network CIDR
func getNotInUseEgressIP(client *gophercloud.ServiceClient, egressCidr string, egressIPNetID string) (string, error) {

	// Obtain the IPs in use in the EgressIP network in Openstack
	egressNetInUseIPs, err := getInUseIPs(client, egressIPNetID)
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

// getEgressIPNetwork returns the IP address from the node egress-ipconfig annotation
func getEgressIPNetwork(node corev1.Node) (string, error) {
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
		return "", nil
	}
	e2e.Logf("Found '%s' annotation in '%s': %s", egressIPConfigAnnotationKey, node.Name, annotation)
	var nodeEgressIPConfigs []*NodeEgressIPConfiguration
	err := json.Unmarshal([]byte(annotation), &nodeEgressIPConfigs)
	if err != nil {
		return "", err
	}
	egressIPNetStr := nodeEgressIPConfigs[0].IFAddr.IPv4
	if egressIPNetStr == "" {
		e2e.Logf("Empty ifaddr.ipv4 in the '%s' annotation", egressIPConfigAnnotationKey)
		return "", nil
	}
	return egressIPNetStr, nil
}

// getNetworkIdFromSubnetCidr returns the Openstack network ID for a given Openstack subnet CIDR
func getNetworkIdFromSubnetCidr(client *gophercloud.ServiceClient, subnetCidr string, infraID string) (string, error) {
	listOpts := subnets.ListOpts{
		CIDR: subnetCidr,
		Tags: "openshiftClusterID=" + infraID,
	}
	allPages, err := subnets.List(client, listOpts).AllPages()
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
	return allSubnets[0].NetworkID, nil
}

// getInUseIPs returns the in use IPs in a given network ID in Openstack
func getInUseIPs(client *gophercloud.ServiceClient, networkId string) ([]string, error) {
	var inUseIPs []string
	portListOpts := ports.ListOpts{
		NetworkID: networkId,
	}
	allPages, err := ports.List(client, portListOpts).AllPages()
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

func getPortsByIP(client *gophercloud.ServiceClient, ipAddr string, networkID string) ([]ports.Port, error) {
	portListOpts := ports.ListOpts{
		FixedIPs:  []ports.FixedIPOpts{{IPAddress: ipAddr}},
		NetworkID: networkID,
	}
	allPages, err := ports.List(client, portListOpts).AllPages()
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
	}, "30s", "1s").Should(o.BeTrue(), "Timed out checking the assigned node '%s' in '%s' CloudPrivateIPConfig.", node, egressIP)
	return true, nil
}

// Returns the list of IPs present on the openstack allowed_address_pairs attribute in the node main port
func getAllowedIPsFromNode(client *gophercloud.ServiceClient, node corev1.Node, machineNetwork string) ([]string, error) {

	result := []string{}
	ip := node.GetAnnotations()["alpha.kubernetes.io/provided-node-ip"]
	nodePorts, err := getPortsByIP(client, ip, machineNetwork)
	if err != nil {
		return nil, err
	}
	if len(nodePorts) != 1 {
		return nil, fmt.Errorf("unexpected number of openstack ports for IP %s", ip)
	}
	for _, addressPair := range nodePorts[0].AllowedAddressPairs {
		result = append(result, addressPair.IPAddress)
	}
	return result, nil
}

// Checking the egressIP is added to allowed_address_pairs in the expected node and it is not present in the other
func checkAllowedAddressesPairs(client *gophercloud.ServiceClient, nodeHoldingEgressIp corev1.Node, nodeNotHoldingEgressIp corev1.Node, egressIp string, networkID string) {

	o.Eventually(func() bool {
		allowedIpList, err := getAllowedIPsFromNode(client, nodeHoldingEgressIp, networkID)
		if err != nil {
			e2e.Logf("error obtaining allowedIpList")
			return false
		} else if !contains(allowedIpList, egressIp) {
			e2e.Logf("egressIP %s still not found on obtained allowedIpList (%s) for node %s", egressIp, allowedIpList, nodeHoldingEgressIp.Name)
			return false
		}
		return true
	}, "10s", "1s").Should(o.BeTrue(), "Timed out checking allowed address pairs for node %s", nodeHoldingEgressIp.Name)
	e2e.Logf("egressIp %s correctly included on the node allowed-address-pairs for %s", egressIp, nodeHoldingEgressIp.Name)

	o.Eventually(func() bool {
		allowedIpList, err := getAllowedIPsFromNode(client, nodeNotHoldingEgressIp, networkID)
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
}
