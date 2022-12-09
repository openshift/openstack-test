package synthetictests

import (
	"github.com/openshift/origin/pkg/test/ginkgo/junitapi"

	"github.com/openshift/origin/pkg/monitor/monitorapi"
)

func testMarketplaceStartupProbeFailure(events monitorapi.Intervals) []*junitapi.JUnitTestCase {
	const testName = "[sig-arch] openshift-marketplace pods should not get excessive startupProbe failures"
	return eventExprMatchThresholdTest(testName, events, marketplaceStartupProbeFailureRegExpStr, duplicateEventThreshold)
}
