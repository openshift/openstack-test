package openstack

import (
	"context"
	"fmt"
	"strings"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
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
	type configParams struct {
		namespace string
		name      string
		key       string
		skip      []string //Slice of properties that are not expected to be present in the internal configMaps
	}

	g.Context("on cloud provider configuration", func() {

		g.BeforeEach(func() {
			g.By("preparing openshift dynamic client")
			clientSet, err = e2e.LoadClientset()
			o.Expect(err).NotTo(o.HaveOccurred())

		})

		// https://bugzilla.redhat.com/show_bug.cgi?id=2065597
		g.It("should haul the user config to the expected config maps", func() {

			userConfigMap := configParams{
				namespace: "openshift-config",
				name:      "cloud-provider-config",
				key:       "config",
				skip:      nil,
			}

			internalConfigMaps := []configParams{
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

			userCfg, err := getConfig(clientSet,
				userConfigMap.namespace,
				userConfigMap.name,
				userConfigMap.key)
			o.Expect(err).NotTo(o.HaveOccurred())

			for _, internalConfigMap := range internalConfigMaps {
				intCfg, err := getConfig(clientSet,
					internalConfigMap.namespace,
					internalConfigMap.name,
					internalConfigMap.key)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By(fmt.Sprintf("Checking configmap: %q for namespace: %q",
					internalConfigMap.name, internalConfigMap.namespace))
				//Iterate over sections, skipping DEFAULT and the sections not present in the user config
				for _, sectionName := range difference(intCfg.SectionStrings(), []string{"DEFAULT"}) {
					if !ElementExists(userCfg.SectionStrings(), sectionName) {
						continue
					}
					usrSection, _ := userCfg.GetSection(sectionName)
					intSection, _ := intCfg.GetSection(sectionName)
					//Iterate over properties, skipping the ones not expected on the internal config
					for _, propertyName := range difference(usrSection.KeyStrings(), internalConfigMap.skip) {
						o.Expect(intSection.KeyStrings()).To(o.ContainElement(propertyName),
							"Expected property %q not found on section %q of configMap %q in namespace %q",
							propertyName, sectionName, internalConfigMap.name, internalConfigMap.namespace)
						usrProperty, _ := usrSection.GetKey(propertyName)
						intProperty, _ := intSection.GetKey(propertyName)
						o.Expect(strings.ToLower(usrProperty.Value())).To(o.Equal(strings.ToLower(intProperty.Value())),
							"Unexpected value for property %q on section %q. configMap '%q', namespace '%q'.",
							propertyName, sectionName, internalConfigMap.name, internalConfigMap.namespace)
						e2e.Logf("  - Property %q with correct value %q on section %q",
							propertyName, intProperty.Value(), sectionName)
					}
				}
			}
		})

		//https://bugzilla.redhat.com/show_bug.cgi?id=2074471
		g.It("should set use-octavia and enabled properties in CCM depending on the NetworkType", func() {

			ccmConfigMap := configParams{
				namespace: "openshift-cloud-controller-manager",
				name:      "cloud-conf",
				key:       "cloud.conf",
				skip:      nil,
			}

			sectionName := "LoadBalancer"
			propertyNames := []string{"use-octavia", "enabled"}

			cfg, err := e2e.LoadConfig()
			o.Expect(err).NotTo(o.HaveOccurred())
			c, err := configv1client.NewForConfig(cfg)
			o.Expect(err).NotTo(o.HaveOccurred())
			network, err := c.ConfigV1().Networks().Get(context.Background(), "cluster", metav1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())

			ccmCfg, err := getConfig(clientSet,
				ccmConfigMap.namespace,
				ccmConfigMap.name,
				ccmConfigMap.key)
			o.Expect(err).NotTo(o.HaveOccurred())

			var ccmPropertyValues []string
			for _, propertyName := range propertyNames {
				value, err := getPropertyValue(sectionName, propertyName, ccmCfg)
				o.Expect(err).NotTo(o.HaveOccurred(), "Error getting the property %q in section %q on configMap "+
					"%q (namespace: %q)", propertyName, sectionName, ccmConfigMap.name, ccmConfigMap.namespace)
				ccmPropertyValues = append(ccmPropertyValues, value)
			}

			g.By(fmt.Sprintf("Checking properties for networkType detected: %q", network.Status.NetworkType))
			var expectedValues []string
			if network.Status.NetworkType == "Kuryr" {
				expectedValues = []string{"true", "false"}
			} else {
				expectedValues = []string{"true", "#UNDEFINED#"}
			}

			o.Expect(ccmPropertyValues).Should(o.Equal(expectedValues),
				"Unexpected values for properties %q on section %q in configMap '%q', namespace '%q' with NetworkType=%s.",
				propertyNames, sectionName, ccmConfigMap.name, ccmConfigMap.namespace, network.Status.NetworkType)
			e2e.Logf("Properties with correct values on section %q in configMap %q (namespace: %q) with NetworkType=%s.",
				sectionName, ccmConfigMap.name, ccmConfigMap.namespace, network.Status.NetworkType)
			e2e.Logf("- Properties (%q): Values (%q)", propertyNames, ccmPropertyValues)
		})
	})
})

// return *ini.File from the 'key' section in the 'cmName' configMap of the specified 'namespace'.
// oc get cm -n {{namespace}} {{cmName}} -o json | jq .data.{{key}}
func getConfig(kubeClient kubernetes.Interface, namespace string, cmName string,
	key string) (*ini.File, error) {

	var cfg *ini.File
	cmClient := kubeClient.CoreV1().ConfigMaps(namespace)
	config, err := cmClient.Get(context.TODO(), cmName, metav1.GetOptions{})
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
