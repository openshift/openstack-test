package test

//go:generate go run -mod vendor ./util/annotate -- ./util/annotate/generated/zz_generated.annotations.go

import (
	_ "github.com/openshift/openstack-test/test/extended/openstack"
)
