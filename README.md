# openstack-test

This repository contains tests specific to OpenShift on OpenStack, based on the [openshift/origin][1] machinery.

The tests sit in [`test/extended/openstack`][2]

Run the tests by exporting both OpenShift and OpenStack credentials, then running `make run`:
1. `export OS_CLOUD=<OS_CLOUD>`
1. `export KUBECONFIG=<kubeconfig>`
1. `make run`

---

## Rebase on origin

The Origin repository is undergoing changes in preparation for the federated testing infrastructure.

Until federated testing is available, this repo must be regularly rebased on Origin.

To rebase:

1. update the dependency with `GONOPROXY=* GONOSUMDB=* go get -d github.com/openshift/origin@<latest-commit-sha>`
1. Update the content of `cmd/openshift-tests` preserving the import of `pkg/cmd/openshift-tests/run` from the current repository
1. Update the content of `pkg` if the logic changes (ignore updates in tests that we don't import)
1. Update the content of `test/extended/util/annotate` if the logic changes
