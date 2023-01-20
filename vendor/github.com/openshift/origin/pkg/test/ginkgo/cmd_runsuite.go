package ginkgo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/openshift/origin/pkg/monitor/intervalcreation"

	"github.com/openshift/origin/pkg/test/ginkgo/junitapi"

	"github.com/openshift/origin/pkg/synthetictests/allowedalerts"

	"github.com/onsi/ginkgo/config"
	"github.com/openshift/origin/pkg/monitor"
	"github.com/openshift/origin/pkg/monitor/monitorapi"
	monitorserialization "github.com/openshift/origin/pkg/monitor/serialization"
	"github.com/openshift/origin/test/extended/util/disruption/controlplane"
	"github.com/openshift/origin/test/extended/util/disruption/frontends"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	// Dump pod displacements with at least 3 instances
	minChainLen = 3

	setupEvent       = "Setup"
	upgradeEvent     = "Upgrade"
	postUpgradeEvent = "PostUpgrade"
)

// Options is used to run a suite of tests by invoking each test
// as a call to a child worker (the run-tests command).
type Options struct {
	Parallelism int
	Count       int
	FailFast    bool
	Timeout     time.Duration
	JUnitDir    string
	TestFile    string
	OutFile     string

	// Regex allows a selection of a subset of tests
	Regex string
	// MatchFn if set is also used to filter the suite contents
	MatchFn func(name string) bool

	// SyntheticEventTests allows the caller to translate events or outside
	// context into a failure.
	SyntheticEventTests JUnitsForEvents

	RunDataWriters []RunDataWriter

	IncludeSuccessOutput bool

	CommandEnv []string

	DryRun        bool
	PrintCommands bool
	Out, ErrOut   io.Writer

	StartTime time.Time
}

func NewOptions() *Options {
	return &Options{
		RunDataWriters: []RunDataWriter{
			// these produce the various intervals.  Different intervals focused on inspecting different problem spaces.
			AdaptEventDataWriter(intervalcreation.NewSpyglassEventIntervalRenderer("everything", intervalcreation.BelongsInEverything)),
			AdaptEventDataWriter(intervalcreation.NewSpyglassEventIntervalRenderer("spyglass", intervalcreation.BelongsInSpyglass)),
			// TODO add visualization of individual apiserver containers and their readiness on this page
			AdaptEventDataWriter(intervalcreation.NewSpyglassEventIntervalRenderer("kube-apiserver", intervalcreation.BelongsInKubeAPIServer)),
			AdaptEventDataWriter(intervalcreation.NewSpyglassEventIntervalRenderer("operators", intervalcreation.BelongsInOperatorRollout)),
			AdaptEventDataWriter(intervalcreation.NewPodEventIntervalRenderer()),

			RunDataWriterFunc(monitor.WriteEventsForJobRun),
			RunDataWriterFunc(monitor.WriteTrackedResourcesForJobRun),
			RunDataWriterFunc(monitor.WriteBackendDisruptionForJobRun),
			RunDataWriterFunc(allowedalerts.WriteAlertDataForJobRun),
		},
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}
}

func (opt *Options) AsEnv() []string {
	var args []string
	args = append(args, fmt.Sprintf("TEST_SUITE_START_TIME=%d", opt.StartTime.Unix()))
	args = append(args, opt.CommandEnv...)
	return args
}

func (opt *Options) SelectSuite(suites []*TestSuite, args []string) (*TestSuite, error) {
	var suite *TestSuite

	// If a test file was provided with no suite, use the "files" suite.
	if len(opt.TestFile) > 0 && len(args) == 0 {
		suite = &TestSuite{
			Name: "files",
		}
	}
	if suite == nil && len(args) == 0 {
		fmt.Fprintf(opt.ErrOut, SuitesString(suites, "Select a test suite to run against the server:\n\n"))
		return nil, fmt.Errorf("specify a test suite to run, for example: %s run %s", filepath.Base(os.Args[0]), suites[0].Name)
	}
	if suite == nil && len(args) > 0 {
		for _, s := range suites {
			if s.Name == args[0] {
				suite = s
				break
			}
		}
	}
	if suite == nil {
		fmt.Fprintf(opt.ErrOut, SuitesString(suites, "Select a test suite to run against the server:\n\n"))
		return nil, fmt.Errorf("suite %q does not exist", args[0])
	}
	// If a test file was provided, override the Matches function
	// to match the tests from both the suite and the file.
	if len(opt.TestFile) > 0 {
		var in []byte
		var err error
		if opt.TestFile == "-" {
			in, err = ioutil.ReadAll(os.Stdin)
			if err != nil {
				return nil, err
			}
		} else {
			in, err = ioutil.ReadFile(opt.TestFile)
		}
		if err != nil {
			return nil, err
		}
		err = matchTestsFromFile(suite, in)
		if err != nil {
			return nil, fmt.Errorf("could not read test suite from input: %v", err)
		}
	}
	return suite, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (opt *Options) Run(suite *TestSuite, junitSuiteName string) error {
	if len(opt.Regex) > 0 {
		if err := filterWithRegex(suite, opt.Regex); err != nil {
			return err
		}
	}
	if opt.MatchFn != nil {
		original := suite.Matches
		suite.Matches = func(name string) bool {
			return original(name) && opt.MatchFn(name)
		}
	}

	syntheticEventTests := JUnitsForAllEvents{
		opt.SyntheticEventTests,
		suite.SyntheticEventTests,
	}

	tests, err := testsForSuite(config.GinkgoConfig)
	if err != nil {
		return err
	}

	// This ensures that tests in the identified paths do not run in parallel, because
	// the test suite reuses shared resources without considering whether another test
	// could be running at the same time. While these are technically [Serial], ginkgo
	// parallel mode provides this guarantee. Doing this for all suites would be too
	// slow.
	setTestExclusion(tests, func(suitePath string, t *testCase) bool {
		for _, name := range []string{
			"/k8s.io/kubernetes/test/e2e/apps/disruption.go",
		} {
			if strings.HasSuffix(suitePath, name) {
				return true
			}
		}
		return false
	})

	tests = suite.Filter(tests)
	if len(tests) == 0 {
		return fmt.Errorf("suite %q does not contain any tests", suite.Name)
	}

	count := opt.Count
	if count == 0 {
		count = suite.Count
	}

	start := time.Now()
	if opt.StartTime.IsZero() {
		opt.StartTime = start
	}

	if opt.PrintCommands {
		status := newTestStatus(opt.Out, true, len(tests), time.Minute, &monitor.Monitor{}, monitor.NewNoOpMonitor(), opt.AsEnv())
		newParallelTestQueue().Execute(context.Background(), tests, 1, status.OutputCommand)
		return nil
	}
	if opt.DryRun {
		for _, test := range sortedTests(tests) {
			fmt.Fprintf(opt.Out, "%q\n", test.name)
		}
		return nil
	}

	if len(opt.JUnitDir) > 0 {
		if _, err := os.Stat(opt.JUnitDir); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("could not access --junit-dir: %v", err)
			}
			if err := os.MkdirAll(opt.JUnitDir, 0755); err != nil {
				return fmt.Errorf("could not create --junit-dir: %v", err)
			}
		}
	}

	parallelism := opt.Parallelism
	if parallelism == 0 {
		parallelism = suite.Parallelism
	}
	if parallelism == 0 {
		parallelism = 10
	}
	timeout := opt.Timeout
	if timeout == 0 {
		timeout = suite.TestTimeout
	}
	if timeout == 0 {
		timeout = 15 * time.Minute
	}

	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()
	abortCh := make(chan os.Signal, 2)
	go func() {
		<-abortCh
		fmt.Fprintf(opt.ErrOut, "Interrupted, terminating tests\n")
		cancelFn()
		sig := <-abortCh
		fmt.Fprintf(opt.ErrOut, "Interrupted twice, exiting (%s)\n", sig)
		switch sig {
		case syscall.SIGINT:
			os.Exit(130)
		default:
			os.Exit(0)
		}
	}()
	signal.Notify(abortCh, syscall.SIGINT, syscall.SIGTERM)

	restConfig, err := monitor.GetMonitorRESTConfig()
	if err != nil {
		return err
	}
	m, err := monitor.Start(ctx, restConfig,
		[]monitor.StartEventIntervalRecorderFunc{
			controlplane.StartAllAPIMonitoring,
			frontends.StartAllIngressMonitoring,
		},
	)
	if err != nil {
		return err
	}

	pc, err := SetupNewPodCollector(ctx)
	if err != nil {
		return err
	}

	pc.SetEvents([]string{setupEvent})
	pc.Run(ctx)

	// if we run a single test, always include success output
	includeSuccess := opt.IncludeSuccessOutput
	if len(tests) == 1 && count == 1 {
		includeSuccess = true
	}

	early, notEarly := splitTests(tests, func(t *testCase) bool {
		return strings.Contains(t.name, "[Early]")
	})

	late, primaryTests := splitTests(notEarly, func(t *testCase) bool {
		return strings.Contains(t.name, "[Late]")
	})

	kubeTests, openshiftTests := splitTests(primaryTests, func(t *testCase) bool {
		return strings.Contains(t.name, "[Suite:k8s]")
	})

	storageTests, kubeTests := splitTests(kubeTests, func(t *testCase) bool {
		return strings.Contains(t.name, "[sig-storage]")
	})

	mustGatherTests, openshiftTests := splitTests(openshiftTests, func(t *testCase) bool {
		return strings.Contains(t.name, "[sig-cli] oc adm must-gather")
	})

	// If user specifies a count, duplicate the kube and openshift tests that many times.
	expectedTestCount := len(early) + len(late)
	if count != -1 {
		originalKube := kubeTests
		originalOpenshift := openshiftTests
		originalStorage := storageTests
		originalMustGather := mustGatherTests

		for i := 1; i < count; i++ {
			kubeTests = append(kubeTests, copyTests(originalKube)...)
			openshiftTests = append(openshiftTests, copyTests(originalOpenshift)...)
			storageTests = append(storageTests, copyTests(originalStorage)...)
			mustGatherTests = append(mustGatherTests, copyTests(originalMustGather)...)
		}
	}
	expectedTestCount += len(openshiftTests) + len(kubeTests) + len(storageTests) + len(mustGatherTests)

	status := newTestStatus(opt.Out, includeSuccess, expectedTestCount, timeout, m, m, opt.AsEnv())
	testCtx := ctx
	if opt.FailFast {
		var cancelFn context.CancelFunc
		testCtx, cancelFn = context.WithCancel(testCtx)
		status.AfterTest(func(t *testCase) {
			if t.failed {
				cancelFn()
			}
		})
	}

	tests = nil

	// run our Early tests
	q := newParallelTestQueue()
	q.Execute(testCtx, early, parallelism, status.Run)
	tests = append(tests, early...)

	// TODO: will move to the monitor
	pc.SetEvents([]string{upgradeEvent})

	// Run kube, storage, openshift, and must-gather tests. If user specified a count of -1,
	// we loop indefinitely.
	for i := 0; (i < 1 || count == -1) && testCtx.Err() == nil; i++ {
		kubeTestsCopy := copyTests(kubeTests)
		q.Execute(testCtx, kubeTestsCopy, parallelism, status.Run)
		tests = append(tests, kubeTestsCopy...)

		// I thought about randomizing the order of the kube, storage, and openshift tests, but storage dominates our e2e runs, so it doesn't help much.
		storageTestsCopy := copyTests(storageTests)
		q.Execute(testCtx, storageTestsCopy, max(1, parallelism/2), status.Run) // storage tests only run at half the parallelism, so we can avoid cloud provider quota problems.
		tests = append(tests, storageTestsCopy...)

		openshiftTestsCopy := copyTests(openshiftTests)
		q.Execute(testCtx, openshiftTestsCopy, parallelism, status.Run)
		tests = append(tests, openshiftTestsCopy...)

		// run the must-gather tests after parallel tests to reduce resource contention
		mustGatherTestsCopy := copyTests(mustGatherTests)
		q.Execute(testCtx, mustGatherTestsCopy, parallelism, status.Run)
		tests = append(tests, mustGatherTestsCopy...)
	}

	// TODO: will move to the monitor
	pc.SetEvents([]string{postUpgradeEvent})

	// run Late test suits after everything else
	q.Execute(testCtx, late, parallelism, status.Run)
	tests = append(tests, late...)

	// TODO: will move to the monitor
	if len(opt.JUnitDir) > 0 {
		pc.ComputePodTransitions()
		data, err := pc.JsonDump()
		if err != nil {
			fmt.Fprintf(opt.ErrOut, "Unable to dump pod placement data: %v\n", err)
		} else {
			if err := ioutil.WriteFile(filepath.Join(opt.JUnitDir, "pod-placement-data.json"), data, 0644); err != nil {
				fmt.Fprintf(opt.ErrOut, "Unable to write pod placement data: %v\n", err)
			}
		}
		chains := pc.PodDisplacements().Dump(minChainLen)
		if err := ioutil.WriteFile(filepath.Join(opt.JUnitDir, "pod-transitions.txt"), []byte(chains), 0644); err != nil {
			fmt.Fprintf(opt.ErrOut, "Unable to write pod placement data: %v\n", err)
		}
	}

	// calculate the effective test set we ran, excluding any incompletes
	tests, _ = splitTests(tests, func(t *testCase) bool { return t.success || t.failed || t.skipped })

	end := time.Now()
	duration := end.Sub(start).Round(time.Second / 10)
	if duration > time.Minute {
		duration = duration.Round(time.Second)
	}

	pass, fail, skip, failing := summarizeTests(tests)

	// monitor the cluster while the tests are running and report any detected anomalies
	var syntheticTestResults []*junitapi.JUnitTestCase
	var syntheticFailure bool
	timeSuffix := fmt.Sprintf("_%s", start.UTC().Format("20060102-150405"))
	events := m.Intervals(time.Time{}, time.Time{})

	if len(opt.JUnitDir) > 0 {
		var additionalEvents monitorapi.Intervals
		filepath.WalkDir(opt.JUnitDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasPrefix(d.Name(), "AdditionalEvents_") {
				return nil
			}
			saved, _ := monitorserialization.EventsFromFile(path)
			additionalEvents = append(additionalEvents, saved...)
			return nil
		})
		if len(additionalEvents) > 0 {
			events = append(events, additionalEvents.Cut(start, end)...)
			sort.Sort(events)
		}
	}

	// add events from alerts so we can create the intervals
	alertEventIntervals, err := monitor.FetchEventIntervalsForAllAlerts(ctx, restConfig, start)
	if err != nil {
		fmt.Printf("\n\n\n#### alertErr=%v\n", err)
	}
	events = append(events, alertEventIntervals...)
	sort.Sort(events)

	events.Clamp(start, end)

	if len(opt.JUnitDir) > 0 {
		if err := opt.WriteRunDataToArtifactsDir(opt.JUnitDir, m, events, timeSuffix); err != nil {
			fmt.Fprintf(opt.ErrOut, "error: Failed to write run-data: %v\n", err)
		}
	}

	if len(events) > 0 {
		var buf *bytes.Buffer
		syntheticTestResults, buf, _ = createSyntheticTestsFromMonitor(events, duration)
		testCases := syntheticEventTests.JUnitsForEvents(events, duration, restConfig, suite.Name)
		syntheticTestResults = append(syntheticTestResults, testCases...)

		if len(syntheticTestResults) > 0 {
			// mark any failures by name
			failing, flaky := sets.NewString(), sets.NewString()
			for _, test := range syntheticTestResults {
				if test.FailureOutput != nil {
					failing.Insert(test.Name)
				}
			}
			// if a test has both a pass and a failure, flag it
			// as a flake
			for _, test := range syntheticTestResults {
				if test.FailureOutput == nil {
					if failing.Has(test.Name) {
						flaky.Insert(test.Name)
					}
				}
			}
			failing = failing.Difference(flaky)
			if failing.Len() > 0 {
				fmt.Fprintf(buf, "Failing invariants:\n\n%s\n\n", strings.Join(failing.List(), "\n"))
				syntheticFailure = true
			}
			if flaky.Len() > 0 {
				fmt.Fprintf(buf, "Flaky invariants:\n\n%s\n\n", strings.Join(flaky.List(), "\n"))
			}
		}

		opt.Out.Write(buf.Bytes())
	}

	// attempt to retry failures to do flake detection
	if fail > 0 && fail <= suite.MaximumAllowedFlakes {
		var retries []*testCase
		for _, test := range failing {
			retry := test.Retry()
			retries = append(retries, retry)
			tests = append(tests, retry)
			if len(retries) > suite.MaximumAllowedFlakes {
				break
			}
		}

		q := newParallelTestQueue()
		status := newTestStatus(ioutil.Discard, opt.IncludeSuccessOutput, len(retries), timeout, m, m, opt.AsEnv())
		q.Execute(testCtx, retries, parallelism, status.Run)
		var flaky []string
		var repeatFailures []*testCase
		for _, test := range retries {
			if test.success {
				flaky = append(flaky, test.name)
			} else {
				repeatFailures = append(repeatFailures, test)
			}
		}
		if len(flaky) > 0 {
			failing = repeatFailures
			sort.Strings(flaky)
			fmt.Fprintf(opt.Out, "Flaky tests:\n\n%s\n\n", strings.Join(flaky, "\n"))
		}
	}

	// report the outcome of the test
	if len(failing) > 0 {
		names := sets.NewString(testNames(failing)...).List()
		fmt.Fprintf(opt.Out, "Failing tests:\n\n%s\n\n", strings.Join(names, "\n"))
	}

	if len(opt.JUnitDir) > 0 {
		if err := writeJUnitReport("junit_e2e", junitSuiteName, tests, opt.JUnitDir, duration, opt.ErrOut, syntheticTestResults...); err != nil {
			fmt.Fprintf(opt.Out, "error: Unable to write e2e JUnit results: %v", err)
		}
	}

	if fail > 0 {
		if len(failing) > 0 || suite.MaximumAllowedFlakes == 0 {
			return fmt.Errorf("%d fail, %d pass, %d skip (%s)", fail, pass, skip, duration)
		}
		fmt.Fprintf(opt.Out, "%d flakes detected, suite allows passing with only flakes\n\n", fail)
	}

	if syntheticFailure {
		return fmt.Errorf("failed because an invariant was violated, %d pass, %d skip (%s)\n", pass, skip, duration)
	}

	fmt.Fprintf(opt.Out, "%d pass, %d skip (%s)\n", pass, skip, duration)
	return ctx.Err()
}
