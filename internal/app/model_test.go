package app

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"
)

func TestModelFactoryForYieldsConfiguredPrimerModel(t *testing.T) {
	t.Parallel()
	want := testModel()
	if got := newModelFactoryFor(want)(); !reflect.DeepEqual(got, want) {
		t.Fatalf("newModelFactoryFor() = %#v, want %#v", got, want)
	}
}

func TestProductionModelsLoaderReturnsEmptyWhenConfigIsAbsent(t *testing.T) {
	t.Parallel()

	got, err := loadProductionModelsFrom(filepath.Join(t.TempDir(), "models.json"), func(model.Model, auth.APIKey) (inference.Client, error) {
		t.Fatal("client factory called for absent configuration")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("loadProductionModelsFrom() error = %v", err)
	}
	if got.PrimerClient != nil || got.PrimerModel.Name != "" || len(got.ACP) != 0 || len(got.Defaults) != 0 || got.ClaudeSmall != "" || got.ConfigRev != "" {
		t.Fatalf("absent configuration returned partial productionModels: %#v", got)
	}
}

func TestProductionModelsLoaderCompilesSecureConfigurationOnce(t *testing.T) {
	const sentinel = "fixture-model-key-not-a-real-secret"
	path := filepath.Join(t.TempDir(), "models.json")
	data := replaceOnce(t, validLMStudioModelConfig, `"provider": "lmstudio"`, `"provider": "openai"`)
	data = replaceOnce(t, string(data), `"api_format": "openai"`, `"api_format": "openai-responses"`)
	data = replaceOnce(t, string(data), `"base_url": "http://localhost:1234/v1"`, `"base_url": ""`)
	data = replaceOnce(t, string(data), `"api_key": ""`, `"api_key": "`+sentinel+`"`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	calls := 0
	got, err := loadProductionModelsFrom(path, func(selected model.Model, key auth.APIKey) (inference.Client, error) {
		calls++
		if string(key) != sentinel {
			t.Fatalf("client key = %q, want sentinel", key)
		}
		return &fakeLLM{credential: string(key)}, nil
	})
	if err != nil {
		t.Fatalf("loadProductionModelsFrom() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("client factory calls = %d, want 1", calls)
	}
	if got.PrimerClient == nil || got.PrimerModel.Name == "" || got.ConfigRev == "" {
		t.Fatalf("compiled production models = %#v", got)
	}
	if got.ConfigRev == sentinel {
		t.Fatal("configuration revision exposed API key")
	}
}
