package app

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	"github.com/looprig/inference/model"
	"github.com/looprig/llm/auto"
)

// newModelFactoryFor builds a ModelFactory over one configured, secret-free
// model identity. The provider credential remains bound only to its client.
func newModelFactoryFor(base model.Model) ModelFactory {
	return func() model.Model { return base }
}

// loadProductionModels is the process-composition boundary for models.json.
// home is the already-resolved looprig base directory (looprigHome's
// result); callers resolve it once from their Config before calling this. An
// absent file yields an empty configuration; callers that require a primer
// reject it before session assembly. Unreadable or invalid files fail here.
func loadProductionModels(home string) (productionModels, error) {
	path, err := defaultModelConfigPath(home)
	if err != nil {
		return productionModels{}, err
	}
	return loadProductionModelsFrom(path, func(selected model.Model, key auth.APIKey) (inference.Client, error) {
		return auto.New(selected, key)
	})
}

func loadProductionModelsFrom(path string, factory configuredClientFactory) (productionModels, error) {
	data, present, err := readModelConfigFile(path)
	if err != nil {
		return productionModels{}, err
	}
	if !present {
		return productionModels{}, nil
	}
	decoded, err := decodeModelConfig(data)
	if err != nil {
		return productionModels{}, err
	}
	normalized, err := normalizeModelConfig(decoded)
	if err != nil {
		return productionModels{}, err
	}
	return compileProductionModels(normalized, factory)
}
