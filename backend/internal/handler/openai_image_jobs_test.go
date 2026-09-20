package handler

import (
	"context"
	"errors"
	"net"
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

func TestForwardImageJobForcesFileURLDelivery(t *testing.T) {
	receivedHeaders := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[]}`))
	}))
	defer server.Close()

	dataDir := t.TempDir()
	cfg := durableImageJobTestConfig(dataDir, 10, 1<<20)
	tcpAddr, ok := server.Listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	cfg.Server.Port = tcpAddr.Port
	store := newOpenAIImageJobStore(cfg)
	jobID := "imgjob_file_url_delivery"
	require.NoError(t, store.create(durableImageJobTestJob(jobID, openAIImageJobStatusRunning, time.Now())))
	h := &OpenAIGatewayHandler{cfg: cfg, imageJobStore: store}
	header := make(http.Header)
	header.Set("X-Image-Result-Delivery", "base64")

	result := h.forwardImageJob(
		context.Background(),
		jobID,
		"/v1/images/generations",
		[]byte(`{"model":"gpt-image-2","prompt":"fake only"}`),
		header,
		5*time.Second,
	)

	require.NoError(t, result.err)
	require.True(t, result.resultStored)
	forwarded := <-receivedHeaders
	require.Equal(t, jobID, forwarded.Get("X-Sub2API-Image-Job-ID"))
	require.Equal(t, "file_url", forwarded.Get("X-Image-Result-Delivery"))
}

func TestServeImageJobPublicFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dataDir := t.TempDir()
	rel := filepath.Join("image_jobs", "imgjob_test", "public", "2026", "07", "16", "sample.png")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, filepath.Dir(rel)), 0o755))
	raw := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, rel), raw, 0o644))

	h := &OpenAIGatewayHandler{imageJobStore: newOpenAIImageJobStore(&config.Config{Pricing: config.PricingConfig{DataDir: dataDir}})}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "filepath", Value: "/" + filepath.ToSlash(rel)}}
	ctx.Request = httptest.NewRequest(http.MethodGet, "/image-files/"+filepath.ToSlash(rel), nil)

	h.ServeImageJobPublicFile(ctx)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Header().Get("Content-Type"), "image/png")
	require.Equal(t, raw, recorder.Body.Bytes())
}

func TestServeImageJobPublicFileRejectsPrivateResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{imageJobStore: newOpenAIImageJobStore(&config.Config{Pricing: config.PricingConfig{DataDir: t.TempDir()}})}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "filepath", Value: "/image_jobs/imgjob_test/result.json"}}
	ctx.Request = httptest.NewRequest(http.MethodGet, "/image-files/image_jobs/imgjob_test/result.json", nil)

	h.ServeImageJobPublicFile(ctx)
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestPreserveImageJobForwardedBaseHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("uses direct caller base for background request", func(t *testing.T) {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18080/v1/image-jobs/images/generations", nil)
		header := make(http.Header)

		preserveImageJobForwardedBaseHeaders(ctx, header)

		require.Equal(t, "127.0.0.1:18080", header.Get("X-Forwarded-Host"))
		require.Equal(t, "http", header.Get("X-Forwarded-Proto"))
	})

	t.Run("keeps public proxy base", func(t *testing.T) {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18080/v1/image-jobs/images/generations", nil)
		header := http.Header{
			"X-Forwarded-Host":  []string{"sub2api.tanzhongyu.asia"},
			"X-Forwarded-Proto": []string{"https"},
		}

		preserveImageJobForwardedBaseHeaders(ctx, header)

		require.Equal(t, "sub2api.tanzhongyu.asia", header.Get("X-Forwarded-Host"))
		require.Equal(t, "https", header.Get("X-Forwarded-Proto"))
	})
}

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
