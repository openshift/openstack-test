
# Update generated artifacts.
#
# Example:
#   make update
update:
	go generate ./test/extended
.PHONY: update

openstack-tests: test/extended/openstack/*
	go build -o $@ ./cmd/openshift-tests

run: openstack-tests
	./$< run --run '\[Feature:openstack\]' openshift/conformance
.PHONY: run

# For backwards compatibility
openstack-test: run
.PHONY: openstack-test
