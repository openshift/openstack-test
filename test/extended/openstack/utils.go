package openstack

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"
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

func GetKuryrNetwork(clientSet *kubernetes.Clientset, namespace string) (KuryrNetwork, error) {
	//TODO(itzikb): Replace a direct call to the API by extending the ClientSet
	knetworkPath := fmt.Sprintf("/apis/openstack.org/v1/namespaces/%s/kuryrnetworks/%s", namespace, namespace)
	data, err := clientSet.RESTClient().
		Get().
		AbsPath(knetworkPath).
		DoRaw(context.TODO())
	var kn KuryrNetwork
	json.Unmarshal(data, &kn)
	return kn, err
}

func GetSubnetIDfromKuryrNetwork(clientSet *kubernetes.Clientset, namespace string) (string, error) {
	kn, err := GetKuryrNetwork(clientSet, namespace)
	if err != nil {
		return "", err
	}
	return kn.Status.SubnetID, nil
}

func CreateNamespace(clientSet *kubernetes.Clientset, baseName string) *v1.Namespace {
	nsName := fmt.Sprintf("%v-%v", baseName, RandomSuffix())
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
		},
	}
	clientSet.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
	e2e.Logf("Namespace %v was created", nsName)

	return ns
}
func DeleteNamespace(clientSet *kubernetes.Clientset, ns *v1.Namespace) {
	err := clientSet.CoreV1().Namespaces().Delete(context.TODO(), ns.Name, metav1.DeleteOptions{})
	if err != nil {
		e2e.Failf("unable to delete namespace %v: %v", ns.Name, err)
	}
}

func CreatePod(clientSet *kubernetes.Clientset, nsName string, baseName string, hostNetwork bool, command []string) (*v1.Pod, error) {
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
	p, err := clientSet.CoreV1().Pods(nsName).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err == nil {
		err = e2epod.WaitTimeoutForPodReadyInNamespace(clientSet, p.Name, nsName, e2e.PodStartShortTimeout)
	}
	return p, err
}
