# openstack-test

This repository contains tests specific to OpenShift on OpenStack, based on the [openshift/origin][1] machinery.

The tests sit in [`test/extended/openstack`][2]

Run the tests by exporting both OpenShift and OpenStack credentials, then running `make run`:
1. `export OS_CLOUD=<OS_CLOUD>`
1. `export KUBECONFIG=<kubeconfig>`
1. `make run`

---

## Implementation details

Origin is referenced as a dependency in `go.mod`. Regularly update it with `go get -d github.com/openshift/origin@<latest-commit-sha>`.

Two Origin packages remain vendored and require manual update:

* the `main` package under `cmd/openshift-tests`;
* the code needed to `go generate` the test list in `test/extended/util/annotate`.

These two are vendored with these changes:

```diff
diff --git a/cmd/openshift-tests/e2e.go b/cmd/openshift-tests/e2e.go
index 2b99f9e7e7..c74c279169 100644
--- a/cmd/openshift-tests/e2e.go
+++ b/cmd/openshift-tests/e2e.go
@@ -10,8 +10,8 @@ import (
 	exutil "github.com/openshift/origin/test/extended/util"
 	"k8s.io/kubectl/pkg/util/templates"
 
-	_ "github.com/openshift/origin/test/extended"
-	_ "github.com/openshift/origin/test/extended/util/annotate/generated"
+	_ "github.com/openshift/openstack-test/test/extended"
+	_ "github.com/openshift/openstack-test/test/extended/util/annotate/generated"
 )
 
 func isDisabled(name string) bool {
diff --git a/test/extended/util/annotate/annotate.go b/test/extended/util/annotate/annotate.go
index ab002b730e..dc244b5574 100644
--- a/test/extended/util/annotate/annotate.go
+++ b/test/extended/util/annotate/annotate.go
@@ -6,7 +6,7 @@ import (
 	"k8s.io/apimachinery/pkg/util/sets"
 	"k8s.io/kubernetes/openshift-hack/e2e/annotate"
 
-	_ "github.com/openshift/origin/test/extended"
+	_ "github.com/openshift/openstack-test/test/extended"
 )
 
 // mergeMaps updates an existing map of string slices with the
```

[1]: https://github.com/openshift/origin
[2]: test/extended/openstack
