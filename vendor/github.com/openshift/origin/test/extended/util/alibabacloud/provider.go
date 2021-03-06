package alibabacloud

import (
	"k8s.io/kubernetes/test/e2e/framework"
)

func init() {
	framework.RegisterProvider("alibabacloud", newProvider)
}

func newProvider() (framework.ProviderInterface, error) {
	return &Provider{}, nil
}

// Provider is a structure to handle alibabacloud for e2e testing
type Provider struct {
	framework.NullProvider
}
