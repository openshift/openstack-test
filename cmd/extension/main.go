package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/component-base/logs"

	"github.com/openshift-eng/openshift-tests-extension/pkg/cmd"
	e "github.com/openshift-eng/openshift-tests-extension/pkg/extension"
	et "github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
	g "github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"
	"github.com/openshift/origin/test/extended/util"
	framework "k8s.io/kubernetes/test/e2e/framework"

	_ "github.com/openshift/openstack-test/test/extended/openstack"
)

func main() {
	pflag.CommandLine = pflag.NewFlagSet("empty", pflag.ExitOnError)
	flag.CommandLine = flag.NewFlagSet("empty", flag.ExitOnError)
	util.InitStandardFlags()
	framework.AfterReadingAllFlags(&framework.TestContext)

	logs.InitLogs()
	defer logs.FlushLogs()

	registry := e.NewRegistry()
	ext := e.NewExtension("openshift", "payload", "openstack-test")

	registerSuites(ext)

	allSpecs, err := g.BuildExtensionTestSpecsFromOpenShiftGinkgoSuite()
	if err != nil {
		panic(fmt.Sprintf("couldn't build extension test specs from ginkgo: %+v", err.Error()))
	}

	componentSpecs := allSpecs.Select(func(spec *et.ExtensionTestSpec) bool {
		for _, loc := range spec.CodeLocations {
			if strings.Contains(loc, "/test/extended/openstack/") && !strings.Contains(loc, "/go/pkg/mod/") && !strings.Contains(loc, "/vendor/") {
				return true
			}
		}
		return false
	})

	componentSpecs.AddBeforeAll(func() {
		if err := util.InitTest(false); err != nil {
			panic(err)
		}
		util.WithCleanup(func() {})
	})

	componentSpecs.Walk(func(spec *et.ExtensionTestSpec) {
		for label := range spec.Labels {
			if strings.HasPrefix(label, "Platform:") {
				platformName := strings.TrimPrefix(label, "Platform:")
				spec.Include(et.PlatformEquals(platformName))
			}
		}

		re := regexp.MustCompile(`\[platform:([a-z]+)\]`)
		if match := re.FindStringSubmatch(spec.Name); match != nil {
			platform := match[1]
			spec.Include(et.PlatformEquals(platform))
		}

		spec.Lifecycle = et.LifecycleInforming
	})

	ext.AddSpecs(componentSpecs)

	registry.Register(ext)

	root := &cobra.Command{
		Long: "Openstack-test Tests",
	}

	root.AddCommand(cmd.DefaultExtensionCommands(registry)...)

	if err := func() error {
		return root.Execute()
	}(); err != nil {
		os.Exit(1)
	}
}

func registerSuites(ext *e.Extension) {
	suites := []e.Suite{
		{
			Name: "openstack-test/conformance/parallel",
			Parents: []string{
				"openshift/conformance/parallel",
			},
			Description: "Parallel conformance tests (Level0, non-serial, non-disruptive)",
			Qualifiers: []string{
				`name.contains("[Level0]") && !(name.contains("[Serial]") || name.contains("[Disruptive]"))`,
			},
		},
		{
			Name: "openstack-test/conformance/serial",
			Parents: []string{
				"openshift/conformance/serial",
			},
			Description: "Serial conformance tests (must run sequentially)",
			Qualifiers: []string{
				`name.contains("[Level0]") && name.contains("[Serial]") && !name.contains("[Disruptive]")`,
			},
		},
		{
			Name:        "openstack-test/disruptive",
			Parents:     []string{"openshift/disruptive"},
			Description: "Disruptive tests (may affect cluster state)",
			Qualifiers: []string{
				`name.contains("[Disruptive]")`,
			},
		},
		{
			Name:        "openstack-test/non-disruptive",
			Description: "All non-disruptive tests (safe for development clusters)",
			Qualifiers: []string{
				`!name.contains("[Disruptive]")`,
			},
		},
		{
			Name:        "openstack-test/all",
			Description: "All openstack-test tests",
		},
	}

	for _, suite := range suites {
		ext.AddSuite(suite)
	}
}
