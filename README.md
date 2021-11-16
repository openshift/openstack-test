# openstack-test

This repository contains tests specific to OpenShift on OpenStack.

The tests sit in [`test/extended/openstack`][2]

Run the tests by:
1. `export OS_CLOUD=<OS_CLOUD>`
1. `export KUBECONFIG=<kubeconfig>`
1. `make run`

The machinery is ported from [openshift/origin][1].

[1]: https://github.com/openshift/origin
[2]: test/extended/openstack
