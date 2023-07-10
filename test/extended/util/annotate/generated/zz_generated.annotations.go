package generated

import (
	"fmt"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
)

var Annotations = map[string]string{
	"[sig-installer][Suite:openshift/openstack] Bugfix bz_2022627: Machine should report all openstack instance addresses": "",

	"[sig-installer][Suite:openshift/openstack] Bugfix bz_2073398: MachineSet scale-in does not leak OpenStack ports": "",

	"[sig-installer][Suite:openshift/openstack] Machine ProviderSpec is correctly applied to OpenStack instances": "",

	"[sig-installer][Suite:openshift/openstack] Machine are in phase Running": "",

	"[sig-installer][Suite:openshift/openstack] MachineSet ProviderSpec template is correctly applied to Machines": "",

	"[sig-installer][Suite:openshift/openstack] MachineSet have role worker": "",

	"[sig-installer][Suite:openshift/openstack] MachineSet replica number corresponds to the number of Machines": "",

	"[sig-installer][Suite:openshift/openstack] The OpenStack platform creates Control plane nodes in a server group": "",

	"[sig-installer][Suite:openshift/openstack] The OpenStack platform creates Control plane nodes on separate hosts when serverGroupPolicy is anti-affinity": "",

	"[sig-installer][Suite:openshift/openstack] The OpenStack platform creates Worker nodes in a server group": "",

	"[sig-installer][Suite:openshift/openstack] The OpenStack platform creates Worker nodes on separate hosts when serverGroupPolicy is anti-affinity": "",

	"[sig-installer][Suite:openshift/openstack] The OpenStack platform on volume creation should follow PVC specs during resizing for prometheus": "",

	"[sig-installer][Suite:openshift/openstack] The Openshift on cloud provider configuration should haul the user config to the expected config maps": "",

	"[sig-installer][Suite:openshift/openstack] The Openshift on cloud provider configuration should set enabled property in [LoadBalancer] section in CCM depending on the NetworkType": "",

	"[sig-installer][Suite:openshift/openstack] The Openshift on cloud provider configuration should store cloud credentials on secrets": "",

	"[sig-installer][Suite:openshift/openstack][Kuryr] Kuryr should create a subnet for a namespace only when a pod without hostNetwork is created in the namespace": "",

	"[sig-installer][Suite:openshift/openstack][egressip] An egressIP attached to a floating IP should be kept after EgressIP node failover with OVN-Kubernetes NetworkType": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should apply lb-method on TCP Amphora LoadBalancer when a TCP svc with monitors and ETP:Local is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should apply lb-method on TCP OVN LoadBalancer when a TCP svc with monitors and ETP:Local is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should apply lb-method on UDP Amphora LoadBalancer when a UDP svc with monitors and ETP:Local is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should apply lb-method on UDP OVN LoadBalancer when a UDP svc with monitors and ETP:Local is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should create a TCP Amphora LoadBalancer when LoadBalancerService ingressController is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should create a TCP Amphora LoadBalancer when a TCP svc with type:LoadBalancer is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should create a TCP OVN LoadBalancer when LoadBalancerService ingressController is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should create a TCP OVN LoadBalancer when a TCP svc with type:LoadBalancer is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should create a UDP Amphora LoadBalancer when a UDP svc with type:LoadBalancer is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should create a UDP OVN LoadBalancer when a UDP svc with type:LoadBalancer is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should create an UDP Amphora LoadBalancer using a pre-created FIP when an UDP LoadBalancer svc setting the LoadBalancerIP spec is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should create an UDP OVN LoadBalancer using a pre-created FIP when an UDP LoadBalancer svc setting the LoadBalancerIP spec is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should limit service access on an UDP Amphora LoadBalancer when an UDP LoadBalancer svc setting the loadBalancerSourceRanges spec is created on Openshift": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should re-use an existing UDP Amphora LoadBalancer when new svc is created on Openshift with the proper annotation": "",

	"[sig-installer][Suite:openshift/openstack][lb][Serial] The Openstack platform should re-use an existing UDP OVN LoadBalancer when new svc is created on Openshift with the proper annotation": "",
}

func init() {
	ginkgo.GetSuite().SetAnnotateFn(func(name string, node types.TestSpec) {
		if newLabels, ok := Annotations[name]; ok {
			node.AppendText(newLabels)
		} else {
			panic(fmt.Sprintf("unable to find test %s", name))
		}
	})
}
