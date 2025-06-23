
openstack-tests: test/extended/openstack/* cmd/openshift-tests/*
	go build -o $@ ./cmd/openshift-tests

# Update generated artifacts.
update:
	mkdir -p ./test/extented/util/annotate/generated
	go generate ./test/extended
.PHONY: update

verify:
	./hack/verify.sh
.PHONY: verify

run: openstack-tests
	./$< run openshift/openstack
.PHONY: run
