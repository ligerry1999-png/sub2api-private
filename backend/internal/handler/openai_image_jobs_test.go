package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
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
