# openstack-test

This repository contains tests specific to OpenShift on OpenStack, based on the [openshift/origin][1] machinery.

The tests sit in [`test/extended/openstack`][2]

Run the tests by exporting both OpenShift and OpenStack credentials, then running `make run`:
1. `export OS_CLOUD=<OS_CLOUD>`
1. `export KUBECONFIG=<kubeconfig>`
1. `make run`

[1]: https://github.com/openshift/origin
[2]: test/extended/openstack

---

## Rebase on Origin

### Step 1: Update Origin as a dependency

Identify the Origin commit you want to rebase `openstack-test` onto.

Origin is referenced as a dependency in `go.mod`. Update it with:
```sh
GONOPROXY=* GONOSUMDB=* go get -d github.com/openshift/origin@<latest-commit-sha>
```

### Step 2: Update Origin dependencies' overrides

In `go.mod`, manually replace all the overrides ("replace") to match Origin's
`go.mod`.

### Step 3: Update Origin's code in openstack-test

We manually vendor Origin code in three packages. To ensure compatibility, manually rebase from Origin and then apply some changes:

```bash
cp ${ORIGIN}/cmd/openshift-tests/openshift-tests.go cmd/openshift-tests/openshift-tests.go
cp ${ORIGIN}/pkg/cmd/openshift-tests/run/*.go pkg/cmd/openshift-tests/run/
cp ${ORIGIN}/test/extended/util/annotate/*.go test/extended/util/annotate/
```

Apply this diff to change Origin's code to use the locally-defined tests:

```diff
diff --git a/cmd/openshift-tests/openshift-tests.go b/cmd/openshift-tests/openshift-tests.go
index 1d06b4145f..292c587263 100644
--- a/cmd/openshift-tests/openshift-tests.go
+++ b/cmd/openshift-tests/openshift-tests.go
@@ -10,6 +10,7 @@ import (
 	"time"
 
 	"github.com/openshift/library-go/pkg/serviceability"
+	"github.com/openshift/openstack-test/pkg/cmd/openshift-tests/run"
 	"github.com/openshift/origin/pkg/cmd"
 	collectdiskcertificates "github.com/openshift/origin/pkg/cmd/openshift-tests/collect-disk-certificates"
 	"github.com/openshift/origin/pkg/cmd/openshift-tests/dev"
@@ -20,7 +21,6 @@ import (
 	"github.com/openshift/origin/pkg/cmd/openshift-tests/monitor/timeline"
 	"github.com/openshift/origin/pkg/cmd/openshift-tests/render"
 	risk_analysis "github.com/openshift/origin/pkg/cmd/openshift-tests/risk-analysis"
-	"github.com/openshift/origin/pkg/cmd/openshift-tests/run"
 	run_disruption "github.com/openshift/origin/pkg/cmd/openshift-tests/run-disruption"
 	run_test "github.com/openshift/origin/pkg/cmd/openshift-tests/run-test"
 	run_upgrade "github.com/openshift/origin/pkg/cmd/openshift-tests/run-upgrade"
diff --git a/pkg/cmd/openshift-tests/run/command.go b/pkg/cmd/openshift-tests/run/command.go
index 57bdd4801e..49dd02a2e4 100644
--- a/pkg/cmd/openshift-tests/run/command.go
+++ b/pkg/cmd/openshift-tests/run/command.go
@@ -4,8 +4,8 @@ import (
 	"context"
 	"fmt"
 
+	"github.com/openshift/openstack-test/pkg/testsuites"
 	"github.com/openshift/origin/pkg/clioptions/imagesetup"
-	"github.com/openshift/origin/pkg/testsuites"
 	"github.com/spf13/cobra"
 	"k8s.io/cli-runtime/pkg/genericclioptions"
 	"k8s.io/kubectl/pkg/util/templates"
diff --git a/test/extended/util/annotate/annotate.go b/test/extended/util/annotate/annotate.go
index 6e47a3dc17..d66399ce77 100644
--- a/test/extended/util/annotate/annotate.go
+++ b/test/extended/util/annotate/annotate.go
@@ -7,7 +7,7 @@ import (
 
 	// this ensures that all origin tests are picked by ginkgo as defined
 	// in test/extended/include.go
-	_ "github.com/openshift/origin/test/extended"
+	_ "github.com/openshift/openstack-test/test/extended"
 )
 
 func main() {
```

 ### Step 4: Tidy up the dependencies

```bash
 go mod tidy && go mod vendor
 ```
