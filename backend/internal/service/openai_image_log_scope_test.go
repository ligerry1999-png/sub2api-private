package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractOpenAIImageLogResultsFromJSONBytes(t *testing.T) {
	results := extractOpenAIImageLogResultsFromResponsesJSONBytes([]byte(`{
		"output": [{"type":"image_generation_call","id":"ig_1","result":"aW1hZ2U=","size":"1024x1024"}],
		"usage": {"output_tokens": 1}
	}`))

	require.Len(t, results, 1)
	require.Equal(t, "aW1hZ2U=", results[0].Result)
	require.Equal(t, "1024x1024", results[0].Size)
}

func TestExtractOpenAIImageLogResultsFromResponsesSSEBodyDeduplicatesFinalOutput(t *testing.T) {
	body := "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_1\",\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"id\":\"ig_1\",\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\"},{\"id\":\"ig_2\",\"type\":\"image_generation_call\",\"result\":\"aW1hZ2Uy\"}]}}\n\n"

	results := extractOpenAIImageLogResultsFromResponsesSSEBody(body)

	require.Len(t, results, 2)
	require.Equal(t, "aW1hZ2U=", results[0].Result)
	require.Equal(t, "aW1hZ2Uy", results[1].Result)
}

func TestExtractOpenAIImageLogResultsFromImagesResponseSupportsURL(t *testing.T) {
	results := extractOpenAIImageLogResultsFromJSONBytes([]byte(`{
		"data": [{"url":"https://images.example.test/generated.png","revised_prompt":"a cat"}]
	}`))

	require.Len(t, results, 1)
	require.Empty(t, results[0].Result)
	require.Equal(t, "https://images.example.test/generated.png", results[0].URL)
	require.Equal(t, "a cat", results[0].RevisedPrompt)
}
