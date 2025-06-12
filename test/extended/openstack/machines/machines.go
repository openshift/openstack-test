package machines

import (
	"context"

	"github.com/stretchr/objx"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/dynamic"
)

const (
	machineAPINamespace   = "openshift-machine-api"
	machineLabelRole      = "machine.openshift.io/cluster-api-machine-role"
	machineAPIGroup       = "machine.openshift.io"
	machineSetOwningLabel = "machine.openshift.io/cluster-api-machineset"
)

func List(ctx context.Context, dc dynamic.Interface, options ...func(*metav1.ListOptions)) ([]objx.Map, error) {
	var listOptions metav1.ListOptions
	for _, apply := range options {
		apply(&listOptions)
	}

	mc := dc.Resource(schema.GroupVersionResource{
		Group:    machineAPIGroup,
		Version:  "v1beta1",
		Resource: "machines",
	}).Namespace(machineAPINamespace)

	obj, err := mc.List(ctx, listOptions)
	if err != nil {
		return nil, err
	}
	return objx.Map(obj.UnstructuredContent()).Get("items").ObjxMapSlice(), nil
}

func ByMachineSet(machineSetName string) func(*metav1.ListOptions) {
	return ByLabel(machineSetOwningLabel, machineSetName)
}

func ByRole(role string) func(*metav1.ListOptions) {
	return ByLabel(machineLabelRole, role)
}

func ByLabel(key, value string) func(*metav1.ListOptions) {
	requirement, err := labels.NewRequirement(key, selection.Equals, []string{value})
	if err != nil {
		// The requirement is malformed; this signals a syntax error
		// that must be fixed in the test code.
		panic(err)
	}
	return ByLabelRequirement(*requirement)
}

func ByLabelRequirement(labelRequirement labels.Requirement) func(*metav1.ListOptions) {
	return func(listOptions *metav1.ListOptions) {
		existingLabelSelector, err := labels.Parse(listOptions.LabelSelector)
		if err != nil {
			// The existing requirements are malformed; this
			// signals a syntax error that must be fixed in the
			// test code.
			panic(err)
		}
		listOptions.LabelSelector = existingLabelSelector.Add(labelRequirement).String()
	}
}

func Get(ctx context.Context, dc dynamic.Interface, name string) (objx.Map, error) {
	mc := dc.Resource(schema.GroupVersionResource{
		Group:    machineAPIGroup,
		Version:  "v1beta1",
		Resource: "machines",
	}).Namespace(machineAPINamespace)

	obj, err := mc.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return objx.Map(obj.UnstructuredContent()), nil
}
