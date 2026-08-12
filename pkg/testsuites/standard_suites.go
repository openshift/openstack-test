package testsuites

import (
	"strings"

	"github.com/openshift/origin/pkg/test/ginkgo"
	"k8s.io/kubectl/pkg/util/templates"

	// these register framework.NewFrameworkExtensions responsible for
	// executing post-test actions, here debug and metrics gathering
	// see https://github.com/kubernetes/kubernetes/blob/v1.26.0/test/e2e/framework/framework.go#L175-L181
	// for more details
	_ "k8s.io/kubernetes/test/e2e/framework/debug/init"
	_ "k8s.io/kubernetes/test/e2e/framework/metrics/init"

	_ "github.com/openshift/openstack-test/test/extended"
	_ "github.com/openshift/openstack-test/test/extended/util/annotate/generated"
)

func StandardTestSuites() []*ginkgo.TestSuite {
	copied := make([]*ginkgo.TestSuite, 0, len(staticSuites))
	for i := range staticSuites {
		curr := staticSuites[i]
		copied = append(copied, &curr)
	}
	return copied
}

// staticSuites are all known test suites this binary should run
var staticSuites = []ginkgo.TestSuite{
	{
		Name: "openshift/openstack",
		Description: templates.LongDesc(`
			Tests that verify OpenStack-specific invariants.`),
		Matches: func(name string) bool {
			return strings.Contains(name, "[Suite:openshift/openstack")
		},
		Parallelism: 30,
	},
}
