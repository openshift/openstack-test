package main

import (
	"strings"

	"github.com/openshift/origin/pkg/test/ginkgo"
	exutil "github.com/openshift/origin/test/extended/util"
	"k8s.io/kubectl/pkg/util/templates"

	_ "github.com/openshift/openstack-test/test/extended"
	_ "github.com/openshift/openstack-test/test/extended/util/annotate/generated"
)

func isDisabled(name string) bool {
	return strings.Contains(name, "[Disabled")
}

type testSuite struct {
	ginkgo.TestSuite

	PreSuite  func(opt *runOptions) error
	PostSuite func(opt *runOptions)

	PreTest func() error
}

type testSuites []testSuite

func (s testSuites) TestSuites() []*ginkgo.TestSuite {
	copied := make([]*ginkgo.TestSuite, 0, len(s))
	for i := range s {
		copied = append(copied, &s[i].TestSuite)
	}
	return copied
}

// staticSuites are all known test suites this binary should run
var staticSuites = testSuites{
	{
		TestSuite: ginkgo.TestSuite{
			Name: "openshift/openstack",
			Description: templates.LongDesc(`
		Tests that verify OpenStack-specific invariants.
		`),
			Matches: func(name string) bool {
				if isDisabled(name) {
					return false
				}
				return strings.Contains(name, "[Suite:openshift/openstack")
			},
			Parallelism: 30,
		},
		PreSuite: suiteWithProviderPreSuite,
	},
}

// isStandardEarlyTest returns true if a test is considered part of the normal
// pre or post condition tests.
func isStandardEarlyTest(name string) bool {
	if !strings.Contains(name, "[Early]") {
		return false
	}
	return strings.Contains(name, "[Suite:openshift/conformance/parallel")
}

// isStandardEarlyOrLateTest returns true if a test is considered part of the normal
// pre or post condition tests.
func isStandardEarlyOrLateTest(name string) bool {
	if !strings.Contains(name, "[Early]") && !strings.Contains(name, "[Late]") {
		return false
	}
	return strings.Contains(name, "[Suite:openshift/conformance/parallel")
}

// suiteWithInitializedProviderPreSuite loads the provider info, but does not
// exclude any tests specific to that provider.
func suiteWithInitializedProviderPreSuite(opt *runOptions) error {
	config, err := decodeProvider(opt.Provider, opt.DryRun, true, nil)
	if err != nil {
		return err
	}
	opt.config = config

	opt.Provider = config.ToJSONString()
	return nil
}

// suiteWithProviderPreSuite ensures that the suite filters out tests from providers
// that aren't relevant (see exutilcluster.ClusterConfig.MatchFn) by loading the
// provider info from the cluster or flags.
func suiteWithProviderPreSuite(opt *runOptions) error {
	if err := suiteWithInitializedProviderPreSuite(opt); err != nil {
		return err
	}
	opt.MatchFn = opt.config.MatchFn()
	return nil
}

// suiteWithNoProviderPreSuite blocks out provider settings from being passed to
// child tests. Used with suites that should not have cloud specific behavior.
func suiteWithNoProviderPreSuite(opt *runOptions) error {
	opt.Provider = `none`
	return suiteWithProviderPreSuite(opt)
}

// suiteWithKubeTestInitialization invokes the Kube suite in order to populate
// data from the environment for the CSI suite. Other suites should use
// suiteWithProviderPreSuite.
func suiteWithKubeTestInitializationPreSuite(opt *runOptions) error {
	if err := suiteWithProviderPreSuite(opt); err != nil {
		return err
	}
	return initializeTestFramework(exutil.TestContext, opt.config, opt.DryRun)
}
