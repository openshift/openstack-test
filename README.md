# openstack-test

This repository contains tests specific to OpenShift on OpenStack, based on the [openshift/origin][1] machinery.

The tests sit in [`test/extended/openstack`][2]

Run the tests by exporting both OpenShift and OpenStack credentials, then running `make run`:

1. `export OS_CLOUD=<OS_CLOUD>`
1. `export KUBECONFIG=<kubeconfig>`
1. `make run`

---

## Rebase on Origin

### Step 1: Update Origin as a dependency

Origin is referenced as a dependency in `go.mod`. Identify the commit
that you want to rebase onto and update it with:

```sh
GONOPROXY=* GONOSUMDB=* go get -d github.com/openshift/origin@<commit-sha>
```

### Step 2: Update go.mod replacements

In `go.mod`, manually update the `replace` directive to match Origin's
`go.mod`

### Step 3: Update openstack-test's main package

We manually vendor Origin code into our tree. To ensure compatibility,
manually copy these files:

```bash
cp ${ORIGIN}/cmd/openshift-tests/openshift-tests.go cmd/openshift-tests/
cp ${ORIGIN}/test/extended/util/annotate/*.go test/extended/util/annotate/
```

Then apply these diffs to the recently copied Origin's files to use the
locally-defined tests:

```diff
diff --git a/cmd/openshift-tests/openshift-tests.go b/../openstack-test/cmd/openshift-tests/openshift-tests.go
index 6121886935..e599cac199 100644
--- a/cmd/openshift-tests/openshift-tests.go
+++ b/../openstack-test/cmd/openshift-tests/openshift-tests.go
@@ -19,7 +19,6 @@ import (
        "github.com/openshift/origin/pkg/cmd/openshift-tests/monitor/timeline"
        "github.com/openshift/origin/pkg/cmd/openshift-tests/render"
        risk_analysis "github.com/openshift/origin/pkg/cmd/openshift-tests/risk-analysis"
-       "github.com/openshift/origin/pkg/cmd/openshift-tests/run"
        run_disruption "github.com/openshift/origin/pkg/cmd/openshift-tests/run-disruption"
        run_test "github.com/openshift/origin/pkg/cmd/openshift-tests/run-test"
        run_upgrade "github.com/openshift/origin/pkg/cmd/openshift-tests/run-upgrade"
@@ -76,7 +75,7 @@ func main() {
        }

        root.AddCommand(
-               run.NewRunCommand(ioStreams),
+               NewRunCommand(ioStreams),
                run_upgrade.NewRunUpgradeCommand(ioStreams),
                images.NewImagesCommand(),
                run_test.NewRunTestCommand(ioStreams),
```

```diff
diff --git a/test/extended/util/annotate/annotate.go b/../openstack-test/test/extended/util/annotate/annotate.go
index 6e47a3dc17..d66399ce77 100644
--- a/test/extended/util/annotate/annotate.go
+++ b/../openstack-test/test/extended/util/annotate/annotate.go
@@ -7,7 +7,7 @@ import (

        // this ensures that all origin tests are picked by ginkgo as defined
        // in test/extended/include.go
-       _ "github.com/openshift/origin/test/extended"
+       _ "github.com/openshift/openstack-test/test/extended"
 )
```

### Step 4: Tidy up dependencies

```bash
GONOPROXY=* GONOSUMDB=* go mod tidy && go mod vendor
```

### Step 5: Check the building

To make sure there is no dependency conflict, try building
`openstack-test` and correct any error that eventually appears.

```bash
make
```

[1]: https://github.com/openshift/origin
[2]: test/extended/openstack
