package app

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

type nonComparableInferenceClient []byte

func (nonComparableInferenceClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("non-comparable test client")
}

func (nonComparableInferenceClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, errors.New("non-comparable test client")
}

func TestModelRoutingClientHandlesNonComparableDuplicateClientsWithoutPanic(t *testing.T) {
	selected := model.CustomModel("fixture", model.APIFormatOpenAI, "https://fixture.test/v1", "fixture")
	client := nonComparableInferenceClient("same-client")

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("newModelRoutingClient panicked for non-comparable client: %v", recovered)
		}
	}()
	if _, err := newModelRoutingClient([]modelBinding{
		{Model: selected, Client: client},
		{Model: selected, Client: client},
	}); err == nil {
		t.Fatal("newModelRoutingClient accepted duplicate non-comparable clients")
	}
}

func TestNewModelRoutingClientRejectsTypedNilClient(t *testing.T) {
	var typedNil *fakeLLM
	selected := model.CustomModel("fixture", model.APIFormatOpenAI, "https://fixture.test/v1", "fixture")
	if _, err := newModelRoutingClient([]modelBinding{{Model: selected, Client: typedNil}}); err == nil {
		t.Fatal("newModelRoutingClient accepted typed-nil inference.Client")
	}
}
