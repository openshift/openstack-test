package openstack

import (
	"context"
	"fmt"
	"strings"

	"github.com/gophercloud/utils/openstack/clientconfig"
	ini "gopkg.in/ini.v1"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
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

	var ctx context.Context
	g.BeforeEach(func() {
		ctx = context.Background()
	})

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

			userCfg, err := getConfig(ctx,
				clientSet,
				userConfigMap.namespace,
				userConfigMap.name,
				userConfigMap.key)
			o.Expect(err).NotTo(o.HaveOccurred())

			for _, internalConfigMap := range internalConfigMaps {
				intCfg, err := getConfig(ctx,
					clientSet,
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
		g.It("should set enabled property in [LoadBalancer] section in CCM depending on the NetworkType", func() {

			ccmConfigMap := configParams{
				namespace: "openshift-cloud-controller-manager",
				name:      "cloud-conf",
				key:       "cloud.conf",
				skip:      nil,
			}

			sectionName := "LoadBalancer"
			propertyNames := []string{"enabled"}

			cfg, err := e2e.LoadConfig()
			o.Expect(err).NotTo(o.HaveOccurred())
			c, err := configv1client.NewForConfig(cfg)
			o.Expect(err).NotTo(o.HaveOccurred())
			network, err := c.ConfigV1().Networks().Get(ctx, "cluster", metav1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred())

			ccmCfg, err := getConfig(ctx,
				clientSet,
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
			// This being a slice is leftover from when we checked more than one property.
			// Might be useful in the future, left as is.
			var expectedValues []string
			if network.Status.NetworkType == "Kuryr" {
				expectedValues = []string{"false"}
			} else {
				expectedValues = []string{"#UNDEFINED#"}
			}

			o.Expect(ccmPropertyValues).Should(o.Equal(expectedValues),
				"Unexpected values for properties %q on section %q in configMap '%q', namespace '%q' with NetworkType=%s.",
				propertyNames, sectionName, ccmConfigMap.name, ccmConfigMap.namespace, network.Status.NetworkType)
			e2e.Logf("Properties with correct values on section %q in configMap %q (namespace: %q) with NetworkType=%s.",
				sectionName, ccmConfigMap.name, ccmConfigMap.namespace, network.Status.NetworkType)
			e2e.Logf("- Properties (%q): Values (%q)", propertyNames, ccmPropertyValues)

		})

		//Reference: https://github.com/openshift/installer/blob/master/docs/user/openstack/README.md#openstack-credentials-update
		g.It("should store cloud credentials on secrets", func() {

			systemNamespace := "kube-system"
			openstackCredsRole := "openstack-creds-secret-reader"
			expectedSecretName := "openstack-credentials"

			g.By(fmt.Sprintf("Getting the secret managed by role %q in %q namespace", openstackCredsRole, systemNamespace))
			role, err := clientSet.RbacV1().Roles(systemNamespace).Get(ctx, openstackCredsRole, metav1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred(), "Error getting role %q in %q namespace", openstackCredsRole, systemNamespace)
			o.Expect(role.Rules[0].ResourceNames[0]).To(o.Equal(expectedSecretName),
				"Unexpected resourceName on role %q in %q namespace", openstackCredsRole, systemNamespace)

			g.By(fmt.Sprintf("Getting the openstack auth url from clouds.conf in secret %q in %q namespace",
				expectedSecretName, systemNamespace))
			secret, err := clientSet.CoreV1().Secrets(systemNamespace).Get(ctx, expectedSecretName, metav1.GetOptions{})
			o.Expect(err).NotTo(o.HaveOccurred(), "Secret %q not found in %q namespace", expectedSecretName, systemNamespace)
			conf, err := ini.Load([]byte(secret.Data["clouds.conf"]))
			o.Expect(err).NotTo(o.HaveOccurred(),
				"clouds.conf key not found on %q secret in %q namespace", expectedSecretName, systemNamespace)
			globalSection, err := conf.GetSection("Global")
			o.Expect(err).NotTo(o.HaveOccurred(),
				"section Global not found on %q secret in %q namespace", expectedSecretName, systemNamespace)
			authUrl, err := globalSection.GetKey("auth-url")
			o.Expect(err).NotTo(o.HaveOccurred(),
				"property auth-url not found on %q secret in %q namespace", expectedSecretName, systemNamespace)

			g.By(fmt.Sprintf("Getting the openstack auth url from clouds.yaml in secret %q in %q namespace", expectedSecretName, systemNamespace))
			cloudsYaml := make(map[string]map[string]*clientconfig.Cloud)
			err = yaml.Unmarshal([]byte(secret.Data["clouds.yaml"]), &cloudsYaml)
			o.Expect(err).NotTo(o.HaveOccurred(),
				"Error unmarshaling clouds.yaml on %q secret in %q namespace", expectedSecretName, systemNamespace)
			clouds := cloudsYaml["clouds"]["openstack"]

			g.By("Compare cloud auth url on secret with openstack API")
			computeClient, err := client(serviceCompute)
			o.Expect(err).NotTo(o.HaveOccurred(), "Error creating openstack client")
			o.Expect(computeClient.IdentityEndpoint).To(o.HavePrefix(authUrl.Value()), "Unexpected auth url on clouds.conf")
			o.Expect(computeClient.IdentityEndpoint).To(o.HavePrefix(clouds.AuthInfo.AuthURL), "Unexpected auth url on clouds.yaml")

		})
	})
})
