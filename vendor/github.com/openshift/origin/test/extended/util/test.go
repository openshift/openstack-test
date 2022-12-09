package util

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"k8s.io/klog/v2"

	kapiv1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	kclientset "k8s.io/client-go/kubernetes"
	rbacv1client "k8s.io/client-go/kubernetes/typed/rbac/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	conformancetestdata "k8s.io/kubernetes/test/conformance/testdata"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/testfiles"
	e2etestingmanifests "k8s.io/kubernetes/test/e2e/testing-manifests"
	testfixtures "k8s.io/kubernetes/test/fixtures"

	// this appears to inexplicably auto-register global flags.
	_ "k8s.io/kubernetes/test/e2e/storage/drivers"

	projectv1 "github.com/openshift/api/project/v1"
	securityv1client "github.com/openshift/client-go/security/clientset/versioned"

	"github.com/openshift/origin/pkg/version"
)

var (
	reportFileName string
	syntheticSuite string
	quiet          bool
)

var TestContext *e2e.TestContextType = &e2e.TestContext

func InitStandardFlags() {
	e2e.RegisterCommonFlags(flag.CommandLine)
	e2e.RegisterClusterFlags(flag.CommandLine)

	// replaced by a bare import above.
	//e2e.RegisterStorageFlags()
}

func InitTest(dryRun bool) error {
	InitDefaultEnvironmentVariables()
	// interpret synthetic input in `--ginkgo.focus` and/or `--ginkgo.skip`
	ginkgo.BeforeEach(checkSyntheticInput)

	TestContext.DeleteNamespace = os.Getenv("DELETE_NAMESPACE") != "false"
	TestContext.VerifyServiceAccount = true
	testfiles.AddFileSource(e2etestingmanifests.GetE2ETestingManifestsFS())
	testfiles.AddFileSource(testfixtures.GetTestFixturesFS())
	testfiles.AddFileSource(conformancetestdata.GetConformanceTestdataFS())
	TestContext.KubectlPath = "kubectl"
	TestContext.KubeConfig = KubeConfigPath()

	// "debian" is used when not set. At least GlusterFS tests need "custom".
	// (There is no option for "rhel" or "centos".)
	TestContext.NodeOSDistro = "custom"
	TestContext.MasterOSDistro = "custom"

	// load and set the host variable for kubectl
	if !dryRun {
		clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: TestContext.KubeConfig}, &clientcmd.ConfigOverrides{})
		cfg, err := clientConfig.ClientConfig()
		if err != nil {
			return err
		}
		TestContext.Host = cfg.Host
	}

	reportFileName = os.Getenv("TEST_REPORT_FILE_NAME")
	if reportFileName == "" {
		reportFileName = "junit"
	}

	quiet = os.Getenv("TEST_OUTPUT_QUIET") == "true"

	// Ensure that Kube tests run privileged (like they do upstream)
	TestContext.CreateTestingNS = createTestingNS

	klog.V(2).Infof("Extended test version %s", version.Get().String())
	return nil
}

var testsStarted bool

// requiresTestStart indicates this code should never be called from within init() or
// Ginkgo test definition.
//
// We explictly prevent Run() from outside of a test because it means that
// test initialization may be expensive. Tests should not vary definition
// based on a cluster, they should be static in definition. Always use framework.Skipf()
// if your test should not be run based on a dynamic condition of the cluster.
func requiresTestStart() {
	if !testsStarted {
		panic("May only be called from within a test case")
	}
}

// WithCleanup instructs utility methods to move out of dry run mode so there are no side
// effects due to package initialization of Ginkgo tests, and then after the function
// completes cleans up any artifacts created by this project.
func WithCleanup(fn func()) {
	testsStarted = true

	// Initialize the fixture directory. If we were the ones to initialize it, set the env
	// var so that child processes inherit this directory and take responsibility for
	// cleaning it up after we exit.
	fixtureDir, init := fixtureDirectory()
	if init {
		os.Setenv("OS_TEST_FIXTURE_DIR", fixtureDir)
		defer func() {
			os.Setenv("OS_TEST_FIXTURE_DIR", "")
			os.RemoveAll(fixtureDir)
		}()
	}

	fn()
}

// InitDefaultEnvironmentVariables makes sure certain required env vars are available
// in the case that extended tests are invoked directly via calls to ginkgo/extended.test
func InitDefaultEnvironmentVariables() {
	if ad := os.Getenv("ARTIFACT_DIR"); len(strings.TrimSpace(ad)) == 0 {
		os.Setenv("ARTIFACT_DIR", filepath.Join(os.TempDir(), "artifacts"))
	}
}

// isGoModulePath returns true if the packagePath reported by reflection is within a
// module and given module path. When go mod is in use, module and modulePath are not
// contiguous as they were in older golang versions with vendoring, so naive contains
// tests fail.
//
// historically: ".../vendor/k8s.io/kubernetes/test/e2e"
// go.mod:       "k8s.io/kubernetes@0.18.4/test/e2e"
func isGoModulePath(packagePath, module, modulePath string) bool {
	return regexp.MustCompile(fmt.Sprintf(`\b%s(@[^/]*|)/%s\b`, regexp.QuoteMeta(module), regexp.QuoteMeta(modulePath))).MatchString(packagePath)
}

func isOriginTest() bool {
	return isGoModulePath(ginkgo.CurrentSpecReport().FileName(), "github.com/openshift/origin", "test")
}

func isKubernetesE2ETest() bool {
	return isGoModulePath(ginkgo.CurrentSpecReport().FileName(), "k8s.io/kubernetes", "test/e2e")
}

func testNameContains(name string) bool {
	return strings.Contains(ginkgo.CurrentSpecReport().FullText(), name)
}

func skipTestNamespaceCustomization() bool {
	return testNameContains("should always delete fast") || testNameContains("should delete fast enough")
}

// createTestingNS ensures that kubernetes e2e tests have their service accounts in the privileged and anyuid SCCs
func createTestingNS(baseName string, c kclientset.Interface, labels map[string]string) (*kapiv1.Namespace, error) {
	if !strings.HasPrefix(baseName, "e2e-") {
		baseName = "e2e-" + baseName
	}

	// we skip SCC if this is a kube e2e test
	isKubeE2ENamespace := strings.HasPrefix(baseName, "e2e-k8s-") || (isKubernetesE2ETest() && !skipTestNamespaceCustomization())
	if isKubeE2ENamespace {
		if labels == nil {
			labels = map[string]string{}
		}
		labels["security.openshift.io/disable-securitycontextconstraints"] = "true"
	}

	ns, err := e2e.CreateTestingNS(baseName, c, labels)
	if err != nil {
		return ns, err
	}

	// Add anyuid and privileged permissions for upstream tests
	if isKubeE2ENamespace {
		clientConfig, err := GetClientConfig(KubeConfigPath())
		if err != nil {
			return ns, err
		}
		securityClient, err := securityv1client.NewForConfig(clientConfig)
		if err != nil {
			return ns, err
		}
		e2e.Logf("About to run a Kube e2e test, ensuring namespace is privileged")
		// add the "privileged" scc to ensure pods that explicitly
		// request extra capabilities are not rejected
		addE2EServiceAccountsToSCC(securityClient, []kapiv1.Namespace{*ns}, "privileged")
		// add the "anyuid" scc to ensure pods that don't specify a
		// uid don't get forced into a range (mimics upstream
		// behavior)
		addE2EServiceAccountsToSCC(securityClient, []kapiv1.Namespace{*ns}, "anyuid")
		// add the "hostmount-anyuid" scc to ensure pods using hostPath
		// can execute tests
		addE2EServiceAccountsToSCC(securityClient, []kapiv1.Namespace{*ns}, "hostmount-anyuid")

		// The intra-pod test requires that the service account have
		// permission to retrieve service endpoints.
		rbacClient, err := rbacv1client.NewForConfig(clientConfig)
		if err != nil {
			return ns, err
		}
		addRoleToE2EServiceAccounts(rbacClient, []kapiv1.Namespace{*ns}, "view")

		// in practice too many kube tests ignore scheduling constraints
		allowAllNodeScheduling(c, ns.Name)
	}

	return ns, err
}

// checkSyntheticInput selects tests based on synthetic skips or focuses
func checkSyntheticInput() {
	checkSuiteSkips()
}

// checkSuiteSkips ensures Origin/Kubernetes synthetic skip labels are applied
// DEPRECATED: remove in a future release
func checkSuiteSkips() {
	suiteConfig, _ := ginkgo.GinkgoConfiguration()
	switch {
	case isOriginTest():
		skip := strings.Join(suiteConfig.SkipStrings, "|")
		if strings.Contains(skip, "Synthetic Origin") {
			ginkgo.Skip("skipping all openshift/origin tests")
		}
	case isKubernetesE2ETest():
		skip := strings.Join(suiteConfig.SkipStrings, "|")
		if strings.Contains(skip, "Synthetic Kubernetes") {
			ginkgo.Skip("skipping all k8s.io/kubernetes tests")
		}
	}
}

var longRetry = wait.Backoff{Steps: 100}

// allowAllNodeScheduling sets the annotation on namespace that allows all nodes to be scheduled onto.
func allowAllNodeScheduling(c kclientset.Interface, namespace string) {
	err := retry.RetryOnConflict(longRetry, func() error {
		ns, err := c.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if ns.Annotations == nil {
			ns.Annotations = make(map[string]string)
		}
		ns.Annotations[projectv1.ProjectNodeSelector] = ""
		_, err = c.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		FatalErr(err)
	}
}

func addE2EServiceAccountsToSCC(securityClient securityv1client.Interface, namespaces []kapiv1.Namespace, sccName string) {
	// Because updates can race, we need to set the backoff retries to be > than the number of possible
	// parallel jobs starting at once. Set very high to allow future high parallelism.
	err := retry.RetryOnConflict(longRetry, func() error {
		scc, err := securityClient.SecurityV1().SecurityContextConstraints().Get(context.Background(), sccName, metav1.GetOptions{})
		if err != nil {
			if apierrs.IsNotFound(err) {
				return nil
			}
			return err
		}

		for _, ns := range namespaces {
			if isE2ENamespace(ns.Name) {
				scc.Groups = append(scc.Groups, fmt.Sprintf("system:serviceaccounts:%s", ns.Name))
			}
		}
		if _, err := securityClient.SecurityV1().SecurityContextConstraints().Update(context.Background(), scc, metav1.UpdateOptions{}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		FatalErr(err)
	}
}

func isE2ENamespace(ns string) bool {
	return true
	//return strings.HasPrefix(ns, "e2e-") ||
	//	strings.HasPrefix(ns, "aggregator-") ||
	//	strings.HasPrefix(ns, "csi-") ||
	//	strings.HasPrefix(ns, "deployment-") ||
	//	strings.HasPrefix(ns, "disruption-") ||
	//	strings.HasPrefix(ns, "gc-") ||
	//	strings.HasPrefix(ns, "kubectl-") ||
	//	strings.HasPrefix(ns, "proxy-") ||
	//	strings.HasPrefix(ns, "provisioning-") ||
	//	strings.HasPrefix(ns, "statefulset-") ||
	//	strings.HasPrefix(ns, "services-")
}

func addRoleToE2EServiceAccounts(rbacClient rbacv1client.RbacV1Interface, namespaces []kapiv1.Namespace, roleName string) {
	err := retry.RetryOnConflict(longRetry, func() error {
		for _, ns := range namespaces {
			if isE2ENamespace(ns.Name) && ns.Status.Phase != kapiv1.NamespaceTerminating {
				_, err := rbacClient.RoleBindings(ns.Name).Create(context.Background(), &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{GenerateName: "default-" + roleName, Namespace: ns.Name},
					RoleRef: rbacv1.RoleRef{
						Kind: "ClusterRole",
						Name: roleName,
					},
					Subjects: []rbacv1.Subject{
						{Name: "default", Namespace: ns.Name, Kind: rbacv1.ServiceAccountKind},
					},
				}, metav1.CreateOptions{})
				if err != nil {
					e2e.Logf("Warning: Failed to add role to e2e service account: %v", err)
				}
			}
		}
		return nil
	})
	if err != nil {
		FatalErr(err)
	}
}
