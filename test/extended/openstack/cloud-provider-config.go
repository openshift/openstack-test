package openstack

import (
	"context"
	"strings"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	ini "gopkg.in/ini.v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[sig-installer][Suite:openshift/openstack] The Openshift", func() {
	defer g.GinkgoRecover()

	var clientSet *kubernetes.Clientset
	var err error

	g.Context("on cloud provider configuration", func() {

		g.BeforeEach(func() {
			g.By("preparing openshift dynamic client")
			clientSet, err = e2e.LoadClientset()
			o.Expect(err).NotTo(o.HaveOccurred())

		})

		// https://bugzilla.redhat.com/show_bug.cgi?id=2065597
		g.It("should haul the user config to the expected config maps", func() {

			type config_params struct {
				namespace string
				name      string
				key       string
				skip      []string //Slice of properties that are not expected to be present in the internal configMaps
			}

			userConfigMap := config_params{
				namespace: "openshift-config",
				name:      "cloud-provider-config",
				key:       "config",
				skip:      nil,
			}

			internalConfigMaps := []config_params{
				{
					namespace: "openshift-config-managed",
					name:      "kube-cloud-config",
					key:       "cloud.conf",
					skip:      []string{"use-octavia"},
				},
				{
					namespace: "openshift-cloud-controller-manager",
					name:      "cloud-conf",
					key:       "cloud.conf",
					skip:      []string{"secret-name", "secret-namespace", "use-octavia"},
				},
				{
					namespace: "openshift-cluster-csi-drivers",
					name:      "cloud-conf",
					key:       "cloud.conf",
					skip:      []string{"secret-name", "secret-namespace", "use-octavia"},
				},
			}

			user_cfg, err := getConfig(clientSet,
				userConfigMap.namespace,
				userConfigMap.name,
				userConfigMap.key)
			o.Expect(err).NotTo(o.HaveOccurred())

			for _, internalConfigMap := range internalConfigMaps {
				int_cfg, err := getConfig(clientSet,
					internalConfigMap.namespace,
					internalConfigMap.name,
					internalConfigMap.key)
				o.Expect(err).NotTo(o.HaveOccurred())

				e2e.Logf("Checking configmap: %q for namespace: %q",
					internalConfigMap.name, internalConfigMap.namespace)
				//Iterate over sections, skipping DEFAULT and the sections not present in the user config
				for _, section_name := range difference(int_cfg.SectionStrings(), []string{"DEFAULT"}) {
					if !ElementExists(user_cfg.SectionStrings(), section_name) {
						continue
					}
					usr_section, _ := user_cfg.GetSection(section_name)
					int_section, _ := int_cfg.GetSection(section_name)
					//Iterate over properties, skipping the ones not expected on the internal config
					for _, property_name := range difference(usr_section.KeyStrings(), internalConfigMap.skip) {
						o.Expect(int_section.KeyStrings()).To(o.ContainElement(property_name),
							"Expected property %q not found on section %q of configMap %q in namespace %q",
							property_name, section_name, internalConfigMap.name, internalConfigMap.namespace)
						usr_property, _ := usr_section.GetKey(property_name)
						int_property, _ := int_section.GetKey(property_name)
						o.Expect(strings.ToLower(usr_property.Value())).To(o.Equal(strings.ToLower(int_property.Value())),
							"Unexpected value for property %q on section %q. configMap '%q', namespace '%q'.",
							property_name, section_name, internalConfigMap.name, internalConfigMap.namespace)
						e2e.Logf("  - Property %q with correct value %q on section %q",
							property_name, int_property.Value(), section_name)
					}
				}
			}
		})
	})
})

// return *ini.File from the 'key' section in the 'cm_name' configMap of the specified 'namespace'.
// oc get cm -n {{namespace}} {{cm_name}} -o json | jq .data.{{key}}
func getConfig(kubeClient kubernetes.Interface, namespace string, cm_name string,
	key string) (*ini.File, error) {

	var cfg *ini.File
	cmClient := kubeClient.CoreV1().ConfigMaps(namespace)
	config, err := cmClient.Get(context.TODO(), cm_name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, err
	}
	cfg, err = ini.Load([]byte(config.Data[key]))
	if errors.IsNotFound(err) {
		return nil, err
	}
	return cfg, nil
}
