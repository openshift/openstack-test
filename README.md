# openstack-test

OpenShift-on-OpenStack end-to-end test suite, built as an [openshift-tests-extension (OTE)][1] plugin.

The tests sit in [`test/extended/openstack`][2].

## Build

```sh
make extension
```

## List tests

```sh
./bin/openstack-test-tests-ext list
```

## Run tests

Export both OpenShift and OpenStack credentials, then:

```sh
export OS_CLOUD=<OS_CLOUD>
export KUBECONFIG=<kubeconfig>
./bin/openstack-test-tests-ext run-suite openstack-test/all
```

[1]: https://github.com/openshift-eng/openshift-tests-extension
[2]: test/extended/openstack
