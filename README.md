# openstack-test

This repository contains tests specific to OpenShift on OpenStack, built as an [OpenShift Tests Extension (OTE)][1].

The tests sit in [`test/extended/openstack`][2].

## Running the tests

Export both OpenShift and OpenStack credentials, then invoke `make run`:
1. `export OS_CLOUD=<OS_CLOUD>`
2. `export KUBECONFIG=<kubeconfig>`
3. `make run`

This builds the extension binary and runs the `openstack-test/all` suite.

### Available suites

| Suite | Description |
|---|---|
| `openstack-test/conformance/parallel` | Parallel conformance tests (Level0, non-serial, non-disruptive) |
| `openstack-test/conformance/serial` | Serial conformance tests (must run sequentially) |
| `openstack-test/disruptive` | Disruptive tests (may affect cluster state) |
| `openstack-test/non-disruptive` | All non-disruptive tests (safe for development clusters) |
| `openstack-test/all` | All openstack-test tests |

To run a specific suite:
```sh
make extension
./bin/openstack-test-tests-ext run-suite openstack-test/conformance/parallel
```

[1]: https://github.com/openshift-eng/openshift-tests-extension
[2]: test/extended/openstack
