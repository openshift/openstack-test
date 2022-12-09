package ginkgo

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"

	"github.com/openshift/origin/pkg/monitor"
	"github.com/openshift/origin/pkg/test/ginkgo/result"
)

type ExitError struct {
	Code int
}

func (e ExitError) Error() string {
	return fmt.Sprintf("exit with code %d", e.Code)
}

// TestOptions handles running a single test.
type TestOptions struct {
	// EnableMonitor is an easy way to enable monitor gathering for a single e2e test.
	// TODO if this is useful enough for general users, we can extend this into an arg, this just ensures the plumbing.
	EnableMonitor        bool
	MonitorEventsOptions *MonitorEventsOptions

	DryRun bool
	Out    io.Writer
	ErrOut io.Writer
}

var _ ginkgo.GinkgoTestingT = &TestOptions{}

func NewTestOptions(out io.Writer, errOut io.Writer) *TestOptions {
	return &TestOptions{
		MonitorEventsOptions: NewMonitorEventsOptions(out, errOut),
		Out:                  out,
		ErrOut:               errOut,
	}
}

func (opt *TestOptions) Run(args []string) error {
	ctx := context.TODO()

	if len(args) != 1 {
		return fmt.Errorf("only a single test name may be passed")
	}

	// Ignore the upstream suite behavior within test execution
	ginkgo.GetSuite().ClearBeforeAndAfterSuiteNodes()
	tests, err := testsForSuite()
	if err != nil {
		return err
	}
	var test *testCase
	for _, t := range tests {
		if t.name == args[0] {
			test = t
			break
		}
	}
	if test == nil {
		return fmt.Errorf("no test exists with that name: %s", args[0])
	}

	if opt.DryRun {
		fmt.Fprintf(opt.Out, "Running test (dry-run)\n")
		return nil
	}

	restConfig, err := monitor.GetMonitorRESTConfig()
	if err != nil {
		return err
	}
	if opt.EnableMonitor {
		_, err = opt.MonitorEventsOptions.Start(ctx, restConfig)
		if err != nil {
			return err
		}
	}

	suiteConfig, reporterConfig := ginkgo.GinkgoConfiguration()
	suiteConfig.FocusStrings = []string{fmt.Sprintf("^ %s$", regexp.QuoteMeta(test.name))}

	// These settings are matched to upstream's ginkgo configuration. See:
	// https://github.com/kubernetes/kubernetes/blob/v1.25.0/test/e2e/framework/test_context.go#L354-L355
	// Turn on EmitSpecProgress to get spec progress (especially on interrupt)
	suiteConfig.EmitSpecProgress = true
	// Randomize specs as well as suites
	suiteConfig.RandomizeAllSpecs = true
	// turn off stdout/stderr capture see https://github.com/kubernetes/kubernetes/pull/111240
	suiteConfig.OutputInterceptorMode = "none"
	// https://github.com/kubernetes/kubernetes/blob/v1.25.0/hack/ginkgo-e2e.sh#L172-L173
	suiteConfig.Timeout = 24 * time.Hour
	reporterConfig.NoColor = true

	ginkgo.SetReporterConfig(reporterConfig)
	ginkgo.GetSuite().RunSpec(test.spec, ginkgo.Labels{}, "", ginkgo.GetFailer(), ginkgo.GetWriter(), suiteConfig)

	if opt.EnableMonitor {
		if err := opt.MonitorEventsOptions.End(ctx, restConfig, ""); err != nil {
			return err
		}
		timeSuffix := fmt.Sprintf("_%s", opt.MonitorEventsOptions.GetStartTime().
			UTC().Format("20060102-150405"))
		if err := opt.MonitorEventsOptions.WriteRunDataToArtifactsDir("", timeSuffix); err != nil {
			fmt.Fprintf(opt.ErrOut, "error: Failed to write run-data: %v\n", err)
		}
	}

	var summary types.SpecReport
	for _, report := range ginkgo.GetSuite().GetReport().SpecReports {
		if report.NumAttempts > 0 {
			summary = report
		}
	}

	switch {
	case summary.State == types.SpecStatePassed:
		if s, ok := result.LastFlake(); ok {
			fmt.Fprintf(opt.ErrOut, "flake: %s\n", s)
			return ExitError{Code: 4}
		}
	case summary.State == types.SpecStateSkipped:
		if len(summary.Failure.Message) > 0 {
			fmt.Fprintf(opt.ErrOut, "skip [%s:%d]: %s\n", lastFilenameSegment(summary.Failure.Location.FileName), summary.Failure.Location.LineNumber, summary.Failure.Message)
		}
		if len(summary.Failure.ForwardedPanic) > 0 {
			fmt.Fprintf(opt.ErrOut, "skip [%s:%d]: %s\n", lastFilenameSegment(summary.Failure.Location.FileName), summary.Failure.Location.LineNumber, summary.Failure.ForwardedPanic)
		}
		return ExitError{Code: 3}
	case summary.State == types.SpecStateFailed, summary.State == types.SpecStatePanicked, summary.State == types.SpecStateInterrupted:
		if len(summary.Failure.ForwardedPanic) > 0 {
			if len(summary.Failure.Location.FullStackTrace) > 0 {
				fmt.Fprintf(opt.ErrOut, "\n%s\n", summary.Failure.Location.FullStackTrace)
			}
			fmt.Fprintf(opt.ErrOut, "fail [%s:%d]: Test Panicked: %s\n", lastFilenameSegment(summary.Failure.Location.FileName), summary.Failure.Location.LineNumber, summary.Failure.ForwardedPanic)
			return ExitError{Code: 1}
		}
		fmt.Fprintf(opt.ErrOut, "fail [%s:%d]: %s\n", lastFilenameSegment(summary.Failure.Location.FileName), summary.Failure.Location.LineNumber, summary.Failure.Message)
		return ExitError{Code: 1}
	default:
		return fmt.Errorf("unrecognized test case outcome: %#v", summary)
	}
	return nil
}

func (opt *TestOptions) Fail() {
	// this function allows us to pass TestOptions as the first argument,
	// it's empty becase we have failure check mechanism implemented above.
}

func lastFilenameSegment(filename string) string {
	if parts := strings.Split(filename, "/vendor/"); len(parts) > 1 {
		return parts[len(parts)-1]
	}
	if parts := strings.Split(filename, "/src/"); len(parts) > 1 {
		return parts[len(parts)-1]
	}
	return filename
}
