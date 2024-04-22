package client

import (
	"context"
	"sync"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
)

var (
	mutex          sync.Mutex
	providerClient *gophercloud.ProviderClient
	endpointOpts   gophercloud.EndpointOpts
)

func getProviderClient(ctx context.Context) (*gophercloud.ProviderClient, gophercloud.EndpointOpts, error) {
	mutex.Lock()
	defer mutex.Unlock()

	if providerClient == nil {
		ao, eo, tlsConfig, err := clouds.Parse()
		if err != nil {
			return nil, gophercloud.EndpointOpts{}, err
		}

		pc, err := config.NewProviderClient(ctx, ao, config.WithTLSConfig(tlsConfig))
		if err != nil {
			return nil, gophercloud.EndpointOpts{}, err
		}
		providerClient, endpointOpts = pc, eo
	}

	return providerClient, endpointOpts, nil
}

func GetServiceClient(ctx context.Context, newServiceClient func(*gophercloud.ProviderClient, gophercloud.EndpointOpts) (*gophercloud.ServiceClient, error)) (*gophercloud.ServiceClient, error) {
	pc, eo, err := getProviderClient(ctx)
	if err != nil {
		return nil, err
	}

	return newServiceClient(pc, eo)
}
