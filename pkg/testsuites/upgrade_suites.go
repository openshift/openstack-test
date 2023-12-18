package testsuites

import (
	"github.com/openshift/origin/pkg/test/ginkgo"
)

func UpgradeTestSuites() []*ginkgo.TestSuite {
	copied := make([]*ginkgo.TestSuite, 0, len(upgradeSuites))
	for i := range upgradeSuites {
		curr := upgradeSuites[i]
		copied = append(copied, &curr)
	}
	return copied
}

// upgradeSuites are all known upgrade test suites this binary should run
var upgradeSuites = []ginkgo.TestSuite{}
