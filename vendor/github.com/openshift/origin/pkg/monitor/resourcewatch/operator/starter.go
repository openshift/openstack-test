package operator

import (
	"context"
	"os"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsv1informer "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"

	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	configv1informer "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"

	"github.com/openshift/origin/pkg/monitor/resourcewatch/controller/clusteroperatormetric"
	"github.com/openshift/origin/pkg/monitor/resourcewatch/controller/configmonitor"
	"github.com/openshift/origin/pkg/monitor/resourcewatch/storage"
)

func RunOperator(ctx context.Context, controllerCtx *controllercmd.ControllerContext) error {
	kubeClient, err := apiextensionsclient.NewForConfig(controllerCtx.ProtoKubeConfig)
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(controllerCtx.KubeConfig)
	if err != nil {
		return err
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(controllerCtx.KubeConfig)
	if err != nil {
		return err
	}

	repositoryPath := "/repository"
	if repositoryPathEnv := os.Getenv("REPOSITORY_PATH"); len(repositoryPathEnv) > 0 {
		repositoryPath = repositoryPathEnv
	}

	configStore, err := storage.NewGitStorage(repositoryPath)
	if err != nil {
		return err
	}

	configClient, err := configv1client.NewForConfig(controllerCtx.KubeConfig)
	if err != nil {
		return err
	}

	configInformer := configv1informer.NewClusterOperatorInformer(configClient, time.Minute, cache.Indexers{})
	crdInformer := apiextensionsv1informer.NewCustomResourceDefinitionInformer(kubeClient, time.Minute, cache.Indexers{})

	openshiftConfigObserver := configmonitor.NewConfigObserverController(
		dynamicClient,
		crdInformer,
		discoveryClient,
		configStore,
		[]schema.GroupVersion{
			{
				Group:   "config.openshift.io", // Track everything under *.config.openshift.io
				Version: "v1",
			},
		},
		[]schema.GroupVersionKind{
			{
				Group:   "apps",
				Version: "v1",
				Kind:    "Deployment",
			},
			{
				Group:   "apps",
				Version: "v1",
				Kind:    "DaemonSet",
			},
			{
				Group:   "",
				Version: "v1",
				Kind:    "Event",
			},
			{
				Group:   "",
				Version: "v1",
				Kind:    "Pod",
			},
			{
				Group:   "",
				Version: "v1",
				Kind:    "Node",
			},
		},
		controllerCtx.EventRecorder,
	)

	clusterOperatorMetric := clusteroperatormetric.NewClusterOperatorMetricController(configInformer, configClient.ConfigV1(), controllerCtx.EventRecorder)

	go crdInformer.Run(ctx.Done())
	go configInformer.Run(ctx.Done())

	go openshiftConfigObserver.Run(ctx, 1)
	go clusterOperatorMetric.Run(ctx, 1)

	<-ctx.Done()

	return nil
}
