package app

import (
	"context"
	"errors"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

var errConfiguredRuntimeClientUnavailable = errors.New("coderig: configured runtime client unavailable")

type modelBinding struct {
	Model  model.Model
	Client inference.Client
}

type modelRoutingClient struct {
	clients map[runtimeModelKey]inference.Client
}

type runtimeModelKey struct {
	Provider  model.ProviderName
	APIFormat model.APIFormat
	BaseURL   string
	Name      string
}

func newModelRoutingClient(bindings []modelBinding) (inference.Client, error) {
	if len(bindings) == 0 {
		return nil, errConfiguredRuntimeClientUnavailable
	}
	router := &modelRoutingClient{clients: make(map[runtimeModelKey]inference.Client, len(bindings))}
	for _, binding := range bindings {
		if binding.Client == nil || binding.Model.Name == "" {
			return nil, errConfiguredRuntimeClientUnavailable
		}
		key := runtimeModelKeyFor(binding.Model)
		if existing, ok := router.clients[key]; ok {
			// Multiple public aliases may intentionally point at the same
			// provider target. They are routable when the composition root
			// bound them to the same credential-bound client; distinct clients
			// would be ambiguous because inference.Request carries the target
			// descriptor, not the public alias.
			if existing != binding.Client {
				return nil, errConfiguredRuntimeClientUnavailable
			}
			continue
		}
		router.clients[key] = binding.Client
	}
	return router, nil
}

func runtimeModelKeyFor(value model.Model) runtimeModelKey {
	return runtimeModelKey{Provider: value.Provider, APIFormat: value.APIFormat, BaseURL: value.BaseURL, Name: value.Name}
}

func (c *modelRoutingClient) clientFor(value model.Model) (inference.Client, error) {
	if c == nil {
		return nil, errConfiguredRuntimeClientUnavailable
	}
	if client, ok := c.clients[runtimeModelKeyFor(value)]; ok {
		return client, nil
	}
	return nil, errConfiguredRuntimeClientUnavailable
}

func (c *modelRoutingClient) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	client, err := c.clientFor(req.Model)
	if err != nil {
		return nil, err
	}
	return client.Invoke(ctx, req)
}

func (c *modelRoutingClient) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	client, err := c.clientFor(req.Model)
	if err != nil {
		return nil, err
	}
	return client.Stream(ctx, req)
}
