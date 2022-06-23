
# Update generated artifacts.
update:
	go generate ./test/extended
.PHONY: update

verify-generated: old:=$(shell mktemp)
verify-generated: new:=test/extended/util/annotate/generated/zz_generated.annotations.go
verify-generated:
	cp '$(new)' '$(old)'
	'$(MAKE)' update
	diff '$(old)' '$(new)'
.PHONY: verify-generated

gofmt_diff=$(shell gofmt -l cmd test)
verify-gofmt:
ifeq (,$(gofmt_diff))
	@true
else
	@echo 'Run: `gofmt -w $(gofmt_diff)`'
	@false
endif
.PHONY: verify-gofmt

verify: verify-gofmt verify-generated
.PHONY: verify

openstack-tests: test/extended/openstack/*
	go build -o $@ ./cmd/openshift-tests

run: openstack-tests
	./$< run --run '\[Feature:openstack\]' openshift/conformance
.PHONY: run
