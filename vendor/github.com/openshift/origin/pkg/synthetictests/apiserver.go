package synthetictests

import (
	"fmt"
	"strings"

	"github.com/openshift/origin/pkg/monitor/monitorapi"
	"github.com/openshift/origin/pkg/test/ginkgo/junitapi"
)

func testPodNodeNameIsImmutable(events monitorapi.Intervals) []*junitapi.JUnitTestCase {
	const testName = "[sig-api-machinery] the pod.spec.nodeName field is immutable, once set cannot be changed"

	failures := []string{}
	for _, event := range events {
		if strings.Contains(event.Message, "pod once assigned to a node must stay on it") {
			failures = append(failures, fmt.Sprintf("%v %v", event.Locator, event.Message))
		}
	}
	if len(failures) == 0 {
		return []*junitapi.JUnitTestCase{{Name: testName}}
	}

	return []*junitapi.JUnitTestCase{{
		Name:      testName,
		SystemOut: strings.Join(failures, "\n"),
		FailureOutput: &junitapi.FailureOutput{
			Output: fmt.Sprintf("Please report in https://bugzilla.redhat.com/show_bug.cgi?id=2042657\n\n%d pods had their immutable field (spec.nodeName) changed\n\n%v", len(failures), strings.Join(failures, "\n")),
		},
	}}
}

func testOauthApiserverProbeErrorLiveness(events monitorapi.Intervals) []*junitapi.JUnitTestCase {
	const testName = "[bz-apiserver-auth] openshift-oauth-apiserver should not get probe error on liveiness probe due to timeout"
	return makeProbeTest(testName, events, probeErrorLivenessMessageRegExpStr, "openshift-oauth-apiserver", duplicateEventThreshold)
}

func testOauthApiserverProbeErrorReadiness(events monitorapi.Intervals) []*junitapi.JUnitTestCase {
	const testName = "[bz-apiserver-auth] openshift-oauth-apiserver should not get probe error on readiiness probe due to timeout"
	return makeProbeTest(testName, events, probeErrorReadinessMessageRegExpStr, "openshift-oauth-apiserver", duplicateEventThreshold)
}

func testOauthApiserverProbeErrorConnectionRefused(events monitorapi.Intervals) []*junitapi.JUnitTestCase {
	const testName = "[bz-apiserver-auth] openshift-oauth-apiserver should not get probe error on readiiness probe due to connection refused"
	return makeProbeTest(testName, events, probeErrorConnectionRefusedRegExpStr, "openshift-oauth-apiserver", duplicateEventThreshold)
}
