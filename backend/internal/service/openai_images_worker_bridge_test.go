package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIGatewayServiceForwardOpenAIImagesViaWorker(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var workerBody []byte
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/v1/images/generations", r.URL.Path)
		require.Equal(t, "bridge-secret", r.Header.Get("X-Internal-Token"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		var err error
		workerBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_worker_123")
		_, _ = w.Write([]byte(`{"created":1710000000,"data":[{"b64_json":"aGVsbG8=","revised_prompt":"draw a cat"}]}`))
	}))
	defer worker.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				ImageWorker: config.GatewayImageWorkerConfig{
					Enabled:         true,
					BaseURL:         worker.URL + "/internal/v1",
					Token:           "bridge-secret",
					FallbackEnabled: true,
					TimeoutSeconds:  5,
				},
			},
		},
	}
	account := &Account{
		ID:       7,
		Name:     "openai-oauth",
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "acct-123",
		},
	}
	parsed := &OpenAIImagesRequest{
		Endpoint:       openAIImagesGenerationsEndpoint,
		Model:          "gpt-image-2",
		Prompt:         "draw a cat",
		N:              1,
		Size:           "1024x1024",
		SizeTier:       "1K",
		ResponseFormat: "b64_json",
	}

	result, err := svc.forwardOpenAIImagesViaWorker(context.Background(), c, account, parsed, "gpt-image-2", "oauth-token-123", time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "req_worker_123", result.RequestID)
	require.Equal(t, 1, result.ImageCount)
	require.Equal(t, "1K", result.ImageSize)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(1710000000), gjson.GetBytes(rec.Body.Bytes(), "created").Int())
	require.Equal(t, "aGVsbG8=", gjson.GetBytes(rec.Body.Bytes(), "data.0.b64_json").String())

	var payload map[string]any
	require.NoError(t, json.Unmarshal(workerBody, &payload))
	require.Equal(t, "oauth-token-123", payload["access_token"])
	require.Equal(t, "acct-123", payload["chatgpt_account_id"])
	require.Equal(t, "gpt-image-2", payload["model"])
	require.Equal(t, "draw a cat", payload["prompt"])
	require.Equal(t, "b64_json", payload["response_format"])
}

func TestBuildOpenAIImagesWorkerInputImagesRejectsRemoteEditURL(t *testing.T) {
	parsed := &OpenAIImagesRequest{
		Endpoint:       openAIImagesEditsEndpoint,
		Prompt:         "edit",
		InputImageURLs: []string{"https://example.com/source.png"},
	}
	_, err := buildOpenAIImagesWorkerInputImages(parsed)
	require.ErrorIs(t, err, errOpenAIImagesWorkerUnsupported)
}

func TestClassifyOpenAIImagesWorkerFailure(t *testing.T) {
	require.Equal(t,
		openAIImagesWorkerFailureTransient,
		classifyOpenAIImagesWorkerFailure(io.ErrUnexpectedEOF),
	)
	require.Equal(t,
		openAIImagesWorkerFailureRateLimited,
		classifyOpenAIImagesWorkerFailure(assertError("You've hit the plus plan limit for image generations requests. You can create more images when the limit resets in 17 hours.")),
	)
	require.Equal(t,
		openAIImagesWorkerFailureTransient,
		classifyOpenAIImagesWorkerFailure(assertError("image worker failed: status=502 message=server_error")),
	)
	require.Equal(t,
		openAIImagesWorkerFailureNoImage,
		classifyOpenAIImagesWorkerFailure(assertError("worker returned no image")),
	)
	require.Equal(t,
		openAIImagesWorkerFailureUnsupported,
		classifyOpenAIImagesWorkerFailure(errOpenAIImagesWorkerUnsupported),
	)
	require.False(t, openAIImagesWorkerShouldSwitchAccount(errOpenAIImagesWorkerUnsupported))
}

func TestOpenAIImagesWorkerCooldownUntilForError(t *testing.T) {
	before := time.Now()
	until := openAIImagesWorkerCooldownUntilForError(assertError("You've hit the plus plan limit for image generations requests. You can create more images when the limit resets in 17 hours and 2 minutes."))
	require.True(t, until.After(before.Add(44*time.Second)), "image worker cooldown should be bounded")
	require.True(t, until.Before(before.Add(46*time.Second)), "image worker cooldown should be bounded to 45 seconds")
}

func TestShouldTryOpenAIImagesWorkerSkipsKnownFailedWorkerDuringNativeFallback(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		ImageWorker: config.GatewayImageWorkerConfig{
			Enabled: true,
			BaseURL: "http://image-worker.internal/v1",
			Token:   "test-token",
		},
	}}}
	parsed := &OpenAIImagesRequest{Endpoint: openAIImagesGenerationsEndpoint}

	require.True(t, svc.shouldTryOpenAIImagesWorker(context.Background(), parsed))
	require.False(t, svc.shouldTryOpenAIImagesWorker(WithOpenAIImageWorkerFallbackAllowed(context.Background()), parsed))
}

type assertError string

func (e assertError) Error() string { return string(e) }
