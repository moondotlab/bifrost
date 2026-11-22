package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestListModelsByKeyResponseShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		wantID      string
		wantOwnedBy string
		wantContext int
	}{
		{
			name:        "OpenAI envelope",
			body:        `{"object":"list","data":[{"id":"gpt-5","owned_by":"openai","context_window":128000}]}`,
			wantID:      "test/gpt-5",
			wantOwnedBy: "openai",
			wantContext: 128000,
		},
		{
			name:        "Together array",
			body:        `[{"id":"zai-org/GLM-5.2","organization":"Z.ai","context_length":131072}]`,
			wantID:      "test/zai-org/GLM-5.2",
			wantOwnedBy: "Z.ai",
			wantContext: 131072,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			response, bifrostErr := ListModelsByKey(
				schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
				&fasthttp.Client{},
				server.URL,
				schemas.Key{Models: schemas.WhiteList{"*"}},
				false,
				nil,
				schemas.ModelProvider("test"),
				false,
				false,
			)

			require.Nil(t, bifrostErr)
			require.Len(t, response.Data, 1)
			require.Equal(t, test.wantID, response.Data[0].ID)
			require.Equal(t, schemas.Ptr(test.wantOwnedBy), response.Data[0].OwnedBy)
			require.Equal(t, schemas.Ptr(test.wantContext), response.Data[0].ContextLength)
		})
	}
}

func TestModelRetrieveResponseShape(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"gpt-6-astra","object":"model","created":1686935002,"owned_by":"openai","shutdown_date":"2026-10-23"}`))
	}))
	defer server.Close()

	response, bifrostErr := HandleOpenAIModelRetrieveRequest(
		schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		&fasthttp.Client{},
		server.URL+"/v1/models/gpt-6-astra",
		schemas.Key{},
		nil,
		schemas.ModelProvider("test"),
		false,
		false,
	)

	require.Nil(t, bifrostErr)
	require.Equal(t, "test/gpt-6-astra", response.ID)
	require.Equal(t, schemas.Ptr("openai"), response.OwnedBy)
	require.Equal(t, schemas.Ptr(int64(1686935002)), response.Created)
	require.Equal(t, schemas.Ptr("2026-10-23"), response.ShutdownDate)
}

func TestModelRetrieveUpstreamErrorIsPropagated(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"The model 'nope' does not exist","type":"invalid_request_error","code":"model_not_found"}}`))
	}))
	defer server.Close()

	response, bifrostErr := HandleOpenAIModelRetrieveRequest(
		schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		&fasthttp.Client{},
		server.URL+"/v1/models/nope",
		schemas.Key{},
		nil,
		schemas.ModelProvider("test"),
		false,
		false,
	)

	require.Nil(t, response)
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusNotFound), bifrostErr.StatusCode)
	require.Contains(t, bifrostErr.Error.Message, "does not exist")
}

func TestOpenAIModelCapabilityJSON(t *testing.T) {
	model := schemas.Model{ID: "model-id"}
	check := func(want string) {
		t.Helper()
		item := ToOpenAIListModelsResponse(&schemas.BifrostListModelsResponse{Data: []schemas.Model{model}}).Data[0]
		data, err := json.Marshal(item)
		require.NoError(t, err)
		require.JSONEq(t, want, string(data))

		retrieved := ToOpenAIModelRetrieveResponse(&schemas.BifrostModelRetrieveResponse{Model: model})
		data, err = json.Marshal(retrieved)
		require.NoError(t, err)
		require.JSONEq(t, want, string(data))
	}
	check(`{"id":"model-id","object":"model","owned_by":""}`)
	model.Mode = "chat"
	model.SupportedEndpoints = []string{"/v1/chat/completions"}
	model.SupportedMethods = []string{string(schemas.ChatCompletionRequest)}
	check(`{"id":"model-id","object":"model","owned_by":"","mode":"chat","supported_endpoints":["/v1/chat/completions"],"supported_methods":["chat_completion"]}`)

	providerModel := ToOpenAIModelRetrieveResponse(&schemas.BifrostModelRetrieveResponse{Model: model})
	retrieved := providerModel.ToBifrostModelRetrieveResponse(schemas.OpenAI)
	listed := (&OpenAIListModelsResponse{Data: []OpenAIModel{*providerModel}}).
		ToBifrostListModelsResponse(schemas.OpenAI, nil, nil, nil, true).Data
	require.Len(t, listed, 1)
	for _, got := range []schemas.Model{retrieved.Model, listed[0]} {
		require.Equal(t, model.Mode, got.Mode)
		require.Equal(t, model.SupportedEndpoints, got.SupportedEndpoints)
		require.Equal(t, model.SupportedMethods, got.SupportedMethods)
	}
}
