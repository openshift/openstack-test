package openstack

import (
	"context"

	"github.com/blang/semver"
	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	exutil "github.com/openshift/origin/test/extended/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

func SkipUnlessOpenStack(ctx context.Context, oc *exutil.CLI) {
	g.By("fetching the cluster platform type")
	infra, err := oc.AdminConfigClient().ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())

	g.By("checking that the platform type is OpenStack")
	if infra.Status.PlatformStatus.Type != configv1.OpenStackPlatformType {
		e2eskipper.Skipf("This test only applies to OpenStack")
	}
}

func SkipUnlessVersion(ctx context.Context, oc *exutil.CLI, version semver.Version) {
	g.By("fetching the cluster version")
	cv, err := oc.AdminConfigClient().ConfigV1().ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	o.Expect(cv.Status.History, err).NotTo(o.BeZero())

	g.By("checking that the version is at least" + version.String())
	for _, v := range cv.Status.History {
		clusterVersion, err := semver.Parse(v.Version)
		o.Expect(err).NotTo(o.HaveOccurred())

		if clusterVersion.GE(version) {
			return
		}
	}
	e2eskipper.Skipf("This test only applies to %q and higher versions", version)
}
