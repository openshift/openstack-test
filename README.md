# openstack-test

This repository contains tests specific to OpenShift on OpenStack.

Run the tests by:
1. `export OS_CLOUD=<OS_CLOUD>`
1. `export KUBECONFIG=<kubeconfig>`
1. `make openstack-test`

The machinery is ported from [openshift/origin][1].

[1]: https:github.com/openshift/origin
