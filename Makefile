
# Update generated artifacts.
update:
	go generate ./test/extended
.PHONY: update

verify:
	./hack/verify.sh
.PHONY: verify

openstack-tests: test/extended/openstack/*
	go build -o $@ ./cmd/openshift-tests

run: openstack-tests
	./$< run openshift/openstack
.PHONY: run
