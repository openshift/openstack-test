package openstack

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/apiversions"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/external"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	machinev1 "github.com/openshift/api/machine/v1beta1"
	operatorv1 "github.com/openshift/api/operator/v1"
	framework "github.com/openshift/cluster-api-actuator-pkg/pkg/framework"
	exutil "github.com/openshift/origin/test/extended/util"
	ini "gopkg.in/ini.v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"
	psapi "k8s.io/pod-security-admission/api"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	NetworkTypeOpenShiftSDN  = "OpenShiftSDN"
	NetworkTypeOVNKubernetes = "OVNKubernetes"
	NetworkTypeKuryr         = "Kuryr"
)

type KuryrNetwork struct {
	Status struct {
		SubnetID string `json:"subnetId"`
	} `json:"status"`
}

func ElementExists(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}

func RandomSuffix() string {
	return strconv.Itoa(rand.Intn(10000))
}

func GetKuryrNetwork(ctx context.Context, clientSet *kubernetes.Clientset, namespace string) (KuryrNetwork, error) {
	//TODO(itzikb): Replace a direct call to the API by extending the ClientSet
	knetworkPath := fmt.Sprintf("/apis/openstack.org/v1/namespaces/%s/kuryrnetworks/%s", namespace, namespace)
	data, err := clientSet.RESTClient().
		Get().
		AbsPath(knetworkPath).
		DoRaw(ctx)
	var kn KuryrNetwork
	json.Unmarshal(data, &kn)
	return kn, err
}

func GetSubnetIDfromKuryrNetwork(ctx context.Context, clientSet *kubernetes.Clientset, namespace string) (string, error) {
	kn, err := GetKuryrNetwork(ctx, clientSet, namespace)
	if err != nil {
		return "", err
	}
	return kn.Status.SubnetID, nil
}

func CreateNamespace(ctx context.Context, clientSet *kubernetes.Clientset, baseName string, privileged bool) *v1.Namespace {
	nsName := fmt.Sprintf("%v-%v", baseName, RandomSuffix())
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
		},
	}
	if privileged {
		ns.Labels = map[string]string{
			psapi.EnforceLevelLabel:                          string(psapi.LevelPrivileged),
			"security.openshift.io/scc.podSecurityLabelSync": "false",
		}
	}
	_, err := clientSet.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		e2e.Failf("unable to create namespace %v: %v", ns.Name, err)
		return nil
	}
	e2e.Logf("Namespace %v was created", nsName)

	return ns
}
func DeleteNamespace(ctx context.Context, clientSet *kubernetes.Clientset, ns *v1.Namespace) {
	err := clientSet.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{})
	if err != nil {
		e2e.Failf("unable to delete namespace %v: %v", ns.Name, err)
	}
}

func CreatePod(ctx context.Context, clientSet *kubernetes.Clientset, nsName string, baseName string, hostNetwork bool, command []string) (*v1.Pod, error) {
	podName := fmt.Sprintf("%v-%v", baseName, RandomSuffix())
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "demo",
					Image:   imageutils.GetE2EImage(imageutils.BusyBox),
					Command: command,
				},
			},
			HostNetwork: hostNetwork,
		},
	}
	p, err := clientSet.CoreV1().Pods(nsName).Create(ctx, pod, metav1.CreateOptions{})
	if err == nil {
		err = e2epod.WaitTimeoutForPodReadyInNamespace(ctx, clientSet, p.Name, nsName, e2e.PodStartShortTimeout)
	}
	return p, err
}

func DeleteMachinesetsDefer(client runtimeclient.Client, ms *machinev1.MachineSet) {
	err := framework.DeleteMachineSets(client, ms)
	if err != nil {
		e2e.Logf("Error occured: %v", err)
	}
}

// difference returns the elements in `a` that aren't in `b`.
func difference(a []string, b []string) []string {
	mb := make(map[string]struct{}, len(b))
	for _, x := range b {
		mb[x] = struct{}{}
	}
	var diff []string
	for _, x := range a {
		if _, found := mb[x]; !found {
			diff = append(diff, x)
		}
	}
	return diff
}

func GetMachinesetRetry(ctx context.Context, client runtimeclient.Client, ms *machinev1.MachineSet, shouldExist bool) error {
	var err error
	const maxRetries = 5
	const delay = 10
	retries := 1
	for retries < maxRetries {
		_, err = framework.GetMachineSet(ctx, client, ms.Name)

		if err != nil == shouldExist {
			retries += 1
			time.Sleep(time.Second * delay)
		} else {
			break
		}
	}
	return err
}

// return *ini.File from the 'key' section in the 'cmName' configMap of the specified 'namespace'.
// oc get cm -n {{namespace}} {{cmName}} -o json | jq .data.{{key}}
func getConfig(ctx context.Context, kubeClient kubernetes.Interface, namespace string, cmName string,
	key string) (*ini.File, error) {

	var cfg *ini.File
	cmClient := kubeClient.CoreV1().ConfigMaps(namespace)
	config, err := cmClient.Get(ctx, cmName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, err
	}
	cfg, err = ini.Load([]byte(config.Data[key]))
	if errors.IsNotFound(err) {
		return nil, err
	}
	return cfg, nil
}

// return an harmonized string with the value of a property defined inside a section.
// return string "#UNDEFINED#" if the property is not defined on the section.
func getPropertyValue(sectionName string, propertyName string, cfg *ini.File) (string, error) {
	section, err := cfg.GetSection(sectionName)
	if err != nil {
		return "", err
	}
	if section.HasKey(propertyName) {
		property, err := section.GetKey(propertyName)
		if err != nil {
			return "", err
		}
		return strings.ToLower(property.Value()), nil
	} else {
		return "#UNDEFINED#", nil
	}
}

func getNetworkType(ctx context.Context, oc *exutil.CLI) (string, error) {
	networks, err := oc.AdminConfigClient().ConfigV1().Networks().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	networkType := networks.Status.NetworkType
	e2e.Logf("Detected network type: %s", networkType)
	return networkType, nil
}

func getMaxOctaviaAPIVersion(client *gophercloud.ServiceClient) (*semver.Version, error) {
	allPages, err := apiversions.List(client).AllPages()
	if err != nil {
		return nil, err
	}

	apiVersions, err := apiversions.ExtractAPIVersions(allPages)
	if err != nil {
		return nil, err
	}

	var max *semver.Version = nil
	for _, apiVersion := range apiVersions {
		ver, err := semver.NewVersion(apiVersion.ID)

		if err != nil {
			// We're ignoring the error, if Octavia is returning anything odd we don't care.
			e2e.Logf("Error when parsing Octavia API version %s: %v. Ignoring it", apiVersion.ID, err)
			continue
		}

		if max == nil || ver.GreaterThan(max) {
			max = ver
		}
	}

	if max == nil {
		// If we have max == nil at this point, then we couldn't read the versions at all.
		max = semver.MustParse("v2.0")
	}

	e2e.Logf("Detected Octavia API: v%s", max)

	return max, nil
}

func IsOctaviaVersionGreaterThanOrEqual(client *gophercloud.ServiceClient, constraint string) (bool, error) {
	maxOctaviaVersion, err := getMaxOctaviaAPIVersion(client)
	if err != nil {
		return false, err
	}

	constraintVer := semver.MustParse(constraint)

	return !constraintVer.GreaterThan(maxOctaviaVersion), nil
}

// GetFloatingNetworkID returns a floating network ID.
func GetFloatingNetworkID(client *gophercloud.ServiceClient, cloudProviderConfig *ini.File) (string, error) {
	configuredNetworkId, _ := GetClusterLoadBalancerSetting("floating-network-id", cloudProviderConfig)
	if configuredNetworkId != "" {
		return configuredNetworkId, nil
	}
	type NetworkWithExternalExt struct {
		networks.Network
		external.NetworkExternalExt
	}
	var allNetworks []NetworkWithExternalExt

	page, err := networks.List(client, networks.ListOpts{}).AllPages()
	if err != nil {
		return "", err
	}

	err = networks.ExtractNetworksInto(page, &allNetworks)
	if err != nil {
		return "", err
	}

	for _, network := range allNetworks {
		if network.External && len(network.Subnets) > 0 {
			page, err := subnets.List(client, subnets.ListOpts{NetworkID: network.ID}).AllPages()
			if err != nil {
				return "", err
			}
			subnetList, err := subnets.ExtractSubnets(page)
			if err != nil {
				return "", err
			}
			for _, networkSubnet := range network.Subnets {
				subnet := getSubnet(networkSubnet, subnetList)
				if subnet != nil {
					if subnet.IPVersion == 4 {
						return network.ID, nil
					}
				} else {
					return network.ID, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no network matching the requirements found")
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func operatorConditionMap(conditions ...operatorv1.OperatorCondition) map[string]string {
	conds := map[string]string{}
	for _, cond := range conditions {
		conds[cond.Type] = string(cond.Status)
	}
	return conds
}

func conditionsMatchExpected(expected, actual map[string]string) bool {
	filtered := map[string]string{}
	for k := range actual {
		if _, comparable := expected[k]; comparable {
			filtered[k] = actual[k]
		}
	}
	return reflect.DeepEqual(expected, filtered)
}

// get the LoadBalancer setting based on the provided CloudProviderConfig INI file and the default values
func GetClusterLoadBalancerSetting(setting string, config *ini.File) (string, error) {

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
