package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCreateImageGenerationJobAccepts440RequestsWithoutExecutingNetwork(t *testing.T) {
	handler, queue, cleanup := newQueuedOpenAIImageJobAdmissionHandler(t, 500, 16<<20)
	defer cleanup()
	seen := make(map[string]struct{}, 440)

	for i := 0; i < 440; i++ {
		recorder := submitFakeOpenAIImageJob(t, handler, "", fmt.Sprintf("fake prompt %03d", i))
		require.Equal(t, http.StatusAccepted, recorder.Code, "submission %d: %s", i, recorder.Body.String())
		var payload struct {
			JobID  string `json:"job_id"`
			Status string `json:"status"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
		require.NotEmpty(t, payload.JobID)
		require.Equal(t, openAIImageJobStatusPending, payload.Status)
		seen[payload.JobID] = struct{}{}
	}
	require.Len(t, seen, 440)

	for range 440 {
		reserved, err := queue.Reserve(context.Background(), 0)
		require.NoError(t, err)
		require.Contains(t, seen, reserved.JobID)
	}
	requireOpenAIImageJobQueueEmpty(t, queue)
}

func TestCreateImageGenerationJobReturns429OnlyWhenDurableQueueIsFull(t *testing.T) {
	handler, queue, cleanup := newQueuedOpenAIImageJobAdmissionHandler(t, 2, 1<<20)
	defer cleanup()

	first := submitFakeOpenAIImageJob(t, handler, "", "first fake")
	second := submitFakeOpenAIImageJob(t, handler, "", "second fake")
	third := submitFakeOpenAIImageJob(t, handler, "", "third fake")

	require.Equal(t, http.StatusAccepted, first.Code, first.Body.String())
	require.Equal(t, http.StatusAccepted, second.Code, second.Body.String())
	require.Equal(t, http.StatusTooManyRequests, third.Code, third.Body.String())
	require.Contains(t, third.Body.String(), "queue")

	for range 2 {
		_, err := queue.Reserve(context.Background(), 0)
		require.NoError(t, err)
	}
	requireOpenAIImageJobQueueEmpty(t, queue)
}

func TestCreateImageGenerationJobIdempotencyReturnsOriginalJob(t *testing.T) {
	handler, queue, cleanup := newQueuedOpenAIImageJobAdmissionHandler(t, 10, 1<<20)
	defer cleanup()

	first := submitFakeOpenAIImageJob(t, handler, "idem-fake-001", "same fake request")
	second := submitFakeOpenAIImageJob(t, handler, "idem-fake-001", "same fake request")
	require.Equal(t, http.StatusAccepted, first.Code, first.Body.String())
	require.Equal(t, http.StatusAccepted, second.Code, second.Body.String())

	firstID := imageJobIDFromAdmissionResponse(t, first)
	secondID := imageJobIDFromAdmissionResponse(t, second)
	require.Equal(t, firstID, secondID)

	reserved, err := queue.Reserve(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, firstID, reserved.JobID)
	requireOpenAIImageJobQueueEmpty(t, queue)
}

func TestCreateImageGenerationJobRejectsIdempotencyKeyReuseWithDifferentRequest(t *testing.T) {
	handler, queue, cleanup := newQueuedOpenAIImageJobAdmissionHandler(t, 10, 1<<20)
	defer cleanup()

	first := submitFakeOpenAIImageJob(t, handler, "idem-fake-conflict", "first fake request")
	conflict := submitFakeOpenAIImageJob(t, handler, "idem-fake-conflict", "different fake request")
	require.Equal(t, http.StatusAccepted, first.Code, first.Body.String())
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())

	reserved, err := queue.Reserve(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, imageJobIDFromAdmissionResponse(t, first), reserved.JobID)
	requireOpenAIImageJobQueueEmpty(t, queue)
}

func newQueuedOpenAIImageJobAdmissionHandler(t *testing.T, maxPending int, maxPendingBytes int64) (*OpenAIGatewayHandler, openAIImageJobQueue, func()) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dataDir := t.TempDir()
	cfg := durableImageJobTestConfig(dataDir, maxPending, maxPendingBytes)
	cfg.Gateway.AsyncImageQueue.ReadyKey = "test:admission:ready"
	cfg.Gateway.AsyncImageQueue.DelayedKey = "test:admission:delayed"
	cfg.Gateway.AsyncImageQueue.ActiveKey = "test:admission:active"
	cfg.Gateway.AsyncImageQueue.PauseKey = "test:admission:paused"
	cfg.Gateway.AsyncImageQueue.InflightKeyPrefix = "test:admission:inflight:"
	cfg.Gateway.AsyncImageQueue.IdempotencyKeyPrefix = "test:admission:idem:"

	queue := newFakeOpenAIImageJobQueue()
	concurrencyService := service.NewConcurrencyService(nil)
	h := &OpenAIGatewayHandler{
		gatewayService:      &service.OpenAIGatewayService{},
		billingCacheService: &service.BillingCacheService{},
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(concurrencyService, SSEPingFormatComment, 0),
		imageJobStore:       newOpenAIImageJobStore(cfg),
		cfg:                 cfg,
	}
	h.imageJobDispatcher = newOpenAIImageJobDispatcher(h, h.imageJobStore, queue, cfg)
	require.NotNil(t, h.imageJobDispatcher)
	cleanup := func() {}
	return h, queue, cleanup
}

func submitFakeOpenAIImageJob(t *testing.T, h *OpenAIGatewayHandler, idempotencyKey string, prompt string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":  "gpt-image-2",
		"prompt": prompt,
	})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/image-jobs/images/generations", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer fake-key-never-sent")
	if idempotencyKey != "" {
		c.Request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:     22,
		UserID: 11,
		Key:    "fake-key-never-sent",
		Status: service.StatusAPIKeyActive,
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 11, Concurrency: 50})

	h.CreateImageGenerationJob(c)
	return recorder
}

func imageJobIDFromAdmissionResponse(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		JobID string `json:"job_id"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.NotEmpty(t, payload.JobID)
	return payload.JobID
}
