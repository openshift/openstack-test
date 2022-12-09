package configmonitor

import (
	"context"
	"fmt"
	"strings"
	"time"

	apiextensionsv1lister "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
)

var (
	defaultResyncDuration = 1 * time.Minute
)

type ConfigObserverController struct {
	crdLister        apiextensionsv1lister.CustomResourceDefinitionLister
	crdInformer      cache.SharedIndexInformer
	dynamicClient    dynamic.Interface
	dynamicInformers []*dynamicConfigInformer
	cachedDiscovery  discovery.CachedDiscoveryInterface
	// monitoredResourceGVs are group+versions we want to monitor all resources beneath.
	monitoredResourceGVs []schema.GroupVersion
	// monitoredResourceGVKs are specific group+version+kinds we want to monitor for. (as opposed to everything in the group)
	monitoredResourceGVKs []schema.GroupVersionKind
	storageHandler        cache.ResourceEventHandler
}

func NewConfigObserverController(
	dynamicClient dynamic.Interface,
	crdInformer cache.SharedIndexInformer,
	discoveryClient *discovery.DiscoveryClient,
	configStorage cache.ResourceEventHandler,
	monitoredResourceGVs []schema.GroupVersion,
	monitoredResourceGVKs []schema.GroupVersionKind,
	recorder events.Recorder,
) factory.Controller {
	c := &ConfigObserverController{
		dynamicClient:         dynamicClient,
		crdInformer:           crdInformer,
		storageHandler:        configStorage,
		monitoredResourceGVs:  monitoredResourceGVs,
		monitoredResourceGVKs: monitoredResourceGVKs,
		cachedDiscovery:       memory.NewMemCacheClient(discoveryClient),
	}
	c.crdLister = apiextensionsv1lister.NewCustomResourceDefinitionLister(c.crdInformer.GetIndexer())

	return factory.New().WithInformers(c.crdInformer).ResyncEvery(defaultResyncDuration).WithSync(c.sync).ToController("ConfigObserverController", recorder.WithComponentSuffix("config-observer-controller"))
}

// currentResourceKinds returns list of group version kind for each resource we want to watch currently.
// We may be watching for CRDs which do not initially exist, thus why this is "current" resource kinds.
func (c *ConfigObserverController) currentResourceKinds() ([]schema.GroupVersionKind, error) {
	observedCrds, err := c.crdLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	var (
		currentConfigResources []schema.GroupVersionKind
		currentKinds           = sets.NewString()
	)
	for _, crd := range observedCrds {
		for _, gv := range c.monitoredResourceGVs {
			if !strings.HasSuffix(crd.GetName(), "."+gv.Group) {
				continue
			}
			for _, version := range crd.Spec.Versions {
				if !version.Served {
					continue
				}
				gvk := schema.GroupVersionKind{
					Group:   gv.Group,
					Version: gv.Version,
					Kind:    crd.Spec.Names.Kind,
				}
				if currentKinds.Has(gvk.Kind) {
					continue
				}
				currentKinds.Insert(gvk.Kind)
				currentConfigResources = append(currentConfigResources, gvk)
			}
		}
		for _, gvk := range c.monitoredResourceGVKs {
			if currentKinds.Has(gvk.Kind) {
				continue
			}
			currentKinds.Insert(gvk.Kind)
			currentConfigResources = append(currentConfigResources, gvk)
		}
	}
	return currentConfigResources, nil
}

func (c *ConfigObserverController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	currentResourcesToMonitor, err := c.currentResourceKinds()
	if err != nil {
		return err
	}

	// TODO: The CRD delete case is not handled. This would require shutting down an observer on a GVK that
	// no longer exists?
	var (
		currentList      []string
		needObserverList []string
		kindNeedObserver []schema.GroupVersionKind
	)
	for _, configKind := range currentResourcesToMonitor {
		currentList = append(currentList, configKind.String())
		hasObserver := false
		for _, o := range c.dynamicInformers {
			if o.isKind(configKind) {
				hasObserver = true
				break
			}
		}
		if !hasObserver {
			kindNeedObserver = append(kindNeedObserver, configKind)
			needObserverList = append(needObserverList, configKind.String())
		}
	}

	var (
		waitForCacheSyncFn  []cache.InformerSynced
		syntheticRequeueErr error
	)

	// If we have new CRD refresh the discovery info and update the mapper
	if len(kindNeedObserver) > 0 {
		// NOTE: this is very time expensive, only do this when we have new kinds
		c.cachedDiscovery.Invalidate()
		gr, err := restmapper.GetAPIGroupResources(c.cachedDiscovery)
		if err != nil {
			return err
		}

		mapper := restmapper.NewDiscoveryRESTMapper(gr)
		for _, kind := range kindNeedObserver {
			mapping, err := mapper.RESTMapping(kind.GroupKind(), kind.Version)
			if err != nil {
				syncCtx.Recorder().Warningf("Unable to find REST mapping for %s/%s: %w", kind.GroupKind().String(), kind.Version, err)
				syntheticRequeueErr = err
				continue
			}

			// we got mapping, lets run the dynamicInformer for the config and install GIT storageHandler event handlers
			dynamicInformer := newDynamicConfigInformer(kind.Kind, mapping.Resource, c.dynamicClient, c.storageHandler)
			waitForCacheSyncFn = append(waitForCacheSyncFn, dynamicInformer.hasSynced)

			go func(k schema.GroupVersionKind) {
				klog.Infof("Starting dynamic informer for %q ...", k.String())
				dynamicInformer.run(ctx)
			}(kind)
			c.dynamicInformers = append(c.dynamicInformers, dynamicInformer)
		}
	}

	cacheCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(cacheCtx.Done(), waitForCacheSyncFn...) {
		return fmt.Errorf("timeout while waiting for dynamic informers to start: %#v", kindNeedObserver)
	}

	return syntheticRequeueErr
}
