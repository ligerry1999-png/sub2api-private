package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestClassifyOpenAIImageJobFailure(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		message       string
		err           error
		wantType      string
		wantRetryable bool
	}{
		{
			name:          "quota exceeded",
			statusCode:    http.StatusTooManyRequests,
			message:       `{"error":{"message":"usage limit reached"}}`,
			wantType:      openAIImageJobErrorQuotaExceeded,
			wantRetryable: false,
		},
		{
			name:          "rate limited",
			statusCode:    http.StatusTooManyRequests,
			message:       "rate limit exceeded",
			wantType:      openAIImageJobErrorRateLimited,
			wantRetryable: true,
		},
		{
			name:          "account auth",
			statusCode:    http.StatusUnauthorized,
			message:       "invalid api key",
			wantType:      openAIImageJobErrorAccountAuth,
			wantRetryable: false,
		},
		{
			name:          "job timeout",
			err:           context.DeadlineExceeded,
			wantType:      openAIImageJobErrorJobTimeout,
			wantRetryable: true,
		},
		{
			name:          "network eof",
			err:           errors.New("unexpected EOF"),
			wantType:      openAIImageJobErrorNetwork,
			wantRetryable: true,
		},
		{
			name:          "upstream 502",
			statusCode:    http.StatusBadGateway,
			message:       "bad gateway",
			wantType:      openAIImageJobErrorUpstreamModel,
			wantRetryable: true,
		},
		{
			name:          "invalid request",
			statusCode:    http.StatusBadRequest,
			message:       "invalid image",
			wantType:      openAIImageJobErrorInvalidRequest,
			wantRetryable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotRetryable := classifyOpenAIImageJobFailure(tt.statusCode, tt.message, tt.err)
			require.Equal(t, tt.wantType, gotType)
			require.Equal(t, tt.wantRetryable, gotRetryable)
		})
	}
}

func TestServeImageJobPublicFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dataDir := t.TempDir()
	rel := filepath.Join("image_jobs", "imgjob_test", "public", "2026", "06", "18", "sample.png")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, filepath.Dir(rel)), 0o755))
	raw := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0, 0, 0, 0, 0}
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, rel), raw, 0o644))

	h := &OpenAIGatewayHandler{
		imageJobStore: newOpenAIImageJobStore(&config.Config{
			Pricing: config.PricingConfig{DataDir: dataDir},
		}),
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "filepath", Value: "/" + filepath.ToSlash(rel)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/image-files/"+filepath.ToSlash(rel), nil)

	h.ServeImageJobPublicFile(c)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "image/png")
	require.Equal(t, raw, rec.Body.Bytes())
}

func TestServeImageJobPublicFileRejectsNonPublicPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{imageJobStore: newOpenAIImageJobStore(&config.Config{})}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "filepath", Value: "/image_jobs/imgjob_test/result.json"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/image-files/image_jobs/imgjob_test/result.json", nil)

	h.ServeImageJobPublicFile(c)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestOpenAIImageJobStoreCancelRunningJob(t *testing.T) {
	store := newOpenAIImageJobStore(&config.Config{
		Pricing: config.PricingConfig{DataDir: t.TempDir()},
	})
	now := time.Now()
	job := &openAIImageJob{
		ID:        "imgjob_cancel_test",
		Status:    openAIImageJobStatusPending,
		Endpoint:  "/v1/images/edits",
		UserID:    1,
		APIKeyID:  2,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, store.create(job))

	canceled := false
	require.True(t, store.registerCancel(job.ID, func() { canceled = true }))
	require.True(t, store.markRunning(job.ID, now.Add(time.Second)))

	updated, changed, err := store.cancel(job.ID, now.Add(2*time.Second))
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, canceled)
	require.Equal(t, openAIImageJobStatusCanceled, updated.Status)
	require.Equal(t, openAIImageJobErrorClientCanceled, updated.ErrorType)
	require.NotNil(t, updated.Retryable)
	require.False(t, *updated.Retryable)

	require.False(t, store.markFailed(job.ID, now.Add(3*time.Second), http.StatusBadGateway, "bad gateway", openAIImageJobErrorUpstreamModel, true))
	loaded, ok := store.get(job.ID)
	require.True(t, ok)
	require.Equal(t, openAIImageJobStatusCanceled, loaded.Status)
}

func TestOpenAIImageJobStoreCancelCompletedJobDoesNotChangeStatus(t *testing.T) {
	store := newOpenAIImageJobStore(&config.Config{
		Pricing: config.PricingConfig{DataDir: t.TempDir()},
	})
	now := time.Now()
	job := &openAIImageJob{
		ID:        "imgjob_done_test",
		Status:    openAIImageJobStatusPending,
		Endpoint:  "/v1/images/edits",
		UserID:    1,
		APIKeyID:  2,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, store.create(job))
	require.NoError(t, store.saveResult(job.ID, http.StatusOK, "application/json", []byte(`{"data":[]}`), now.Add(time.Second)))

	updated, changed, err := store.cancel(job.ID, now.Add(2*time.Second))
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, openAIImageJobStatusSuccess, updated.Status)
}
