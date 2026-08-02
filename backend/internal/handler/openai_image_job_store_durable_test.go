package handler

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAIImageJobStoreRejectsOnlyWhenDurableCapacityIsFull(t *testing.T) {
	dataDir := t.TempDir()
	cfg := durableImageJobTestConfig(dataDir, 2, 1<<20)
	store := newOpenAIImageJobStore(cfg)
	now := time.Now().UTC()

	require.NoError(t, store.createWithRequest(
		durableImageJobTestJob("imgjob_capacity_1", openAIImageJobStatusPending, now),
		durableImageJobTestRequest(),
	))
	require.NoError(t, store.createWithRequest(
		durableImageJobTestJob("imgjob_capacity_2", openAIImageJobStatusRunning, now),
		durableImageJobTestRequest(),
	))

	err := store.createWithRequest(
		durableImageJobTestJob("imgjob_capacity_3", openAIImageJobStatusPending, now),
		durableImageJobTestRequest(),
	)
	require.ErrorIs(t, err, errOpenAIImageJobQueueCapacity)

	require.True(t, store.markFailed(
		"imgjob_capacity_2",
		now.Add(time.Second),
		http.StatusBadRequest,
		"permanent invalid request",
		openAIImageJobErrorInvalidRequest,
		false,
	))
	require.NoError(t, store.createWithRequest(
		durableImageJobTestJob("imgjob_capacity_3", openAIImageJobStatusPending, now.Add(2*time.Second)),
		durableImageJobTestRequest(),
	))
}

func TestOpenAIImageJobStoreEnforcesPendingByteCapacity(t *testing.T) {
	dataDir := t.TempDir()
	request := durableImageJobTestRequest()
	request.Body = []byte(`{"model":"gpt-image-2","prompt":"this fake body consumes durable capacity"}`)
	store := newOpenAIImageJobStore(durableImageJobTestConfig(dataDir, 10, int64(len(request.Body))))
	now := time.Now().UTC()

	require.NoError(t, store.createWithRequest(
		durableImageJobTestJob("imgjob_bytes_1", openAIImageJobStatusPending, now),
		request,
	))
	err := store.createWithRequest(
		durableImageJobTestJob("imgjob_bytes_2", openAIImageJobStatusPending, now),
		request,
	)
	require.True(t, errors.Is(err, errOpenAIImageJobQueueCapacity))
}

func TestOpenAIImageJobStoreCreateWithRequestPersistsSafePayload(t *testing.T) {
	dataDir := t.TempDir()
	store := newOpenAIImageJobStore(&config.Config{
		Pricing: config.PricingConfig{DataDir: dataDir},
	})
	now := time.Now().UTC()
	job := durableImageJobTestJob("imgjob_durable_payload", openAIImageJobStatusPending, now)
	request := &openAIImageJobRequest{
		Endpoint:    "/v1/images/generations",
		ContentType: "application/json",
		Header: http.Header{
			"Authorization":     []string{"Bearer must-not-be-persisted"},
			"X-Forwarded-Host":  []string{"images.example.test"},
			"X-Forwarded-Proto": []string{"https"},
		},
		Body: []byte(`{"model":"gpt-image-2","prompt":"durable fake request"}`),
	}

	require.NoError(t, store.createWithRequest(job, request))

	reloadedStore := newOpenAIImageJobStore(&config.Config{
		Pricing: config.PricingConfig{DataDir: dataDir},
	})
	reloaded, err := reloadedStore.readRequest(job.ID)
	require.NoError(t, err)
	require.Equal(t, request.Endpoint, reloaded.Endpoint)
	require.Equal(t, request.ContentType, reloaded.ContentType)
	require.JSONEq(t, string(request.Body), string(reloaded.Body))
	require.Empty(t, reloaded.Header.Get("Authorization"))
	require.Equal(t, "images.example.test", reloaded.Header.Get("X-Forwarded-Host"))
	require.Equal(t, "https", reloaded.Header.Get("X-Forwarded-Proto"))

	for _, privateFile := range []string{"meta.json", "request.json", "request.bin"} {
		info, err := os.Stat(filepath.Join(store.jobDir(job.ID), privateFile))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "%s can contain private prompts and must not be world-readable", privateFile)
	}

	entries, err := os.ReadDir(store.jobDir(job.ID))
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".tmp", "atomic write must not leave a temporary payload behind")
	}
}

func TestOpenAIImageJobStoreCleanupKeepsOldRecoverableJobs(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("IMAGE_JOBS_RETENTION_DAYS", "1")
	store := newOpenAIImageJobStore(&config.Config{
		Pricing: config.PricingConfig{DataDir: dataDir},
	})
	old := time.Now().UTC().Add(-72 * time.Hour)

	for _, status := range []string{openAIImageJobStatusPending, openAIImageJobStatusRunning} {
		job := durableImageJobTestJob("imgjob_cleanup_"+status, status, old)
		require.NoError(t, store.createWithRequest(job, durableImageJobTestRequest()))
		require.NoError(t, os.Chtimes(store.jobDir(job.ID), old, old))
	}

	store.cleanupExpired()

	for _, status := range []string{openAIImageJobStatusPending, openAIImageJobStatusRunning} {
		jobID := "imgjob_cleanup_" + status
		job, ok := store.get(jobID)
		require.True(t, ok, "%s job must survive retention cleanup", status)
		require.Equal(t, status, job.Status)
		_, err := store.readRequest(jobID)
		require.NoError(t, err)
	}
}

func TestOpenAIImageJobStoreListsPendingAndInterruptedRunningJobsAfterRestart(t *testing.T) {
	dataDir := t.TempDir()
	cfg := &config.Config{Pricing: config.PricingConfig{DataDir: dataDir}}
	store := newOpenAIImageJobStore(cfg)
	now := time.Now().UTC()

	for _, status := range []string{openAIImageJobStatusPending, openAIImageJobStatusRunning, openAIImageJobStatusSuccess, openAIImageJobStatusFailed, openAIImageJobStatusCanceled} {
		job := durableImageJobTestJob("imgjob_restart_"+status, status, now)
		require.NoError(t, store.createWithRequest(job, durableImageJobTestRequest()))
	}

	restartedStore := newOpenAIImageJobStore(cfg)
	recoverable, err := restartedStore.listRecoverable()
	require.NoError(t, err)
	ids := make([]string, 0, len(recoverable))
	for _, job := range recoverable {
		ids = append(ids, job.ID)
		_, err := restartedStore.readRequest(job.ID)
		require.NoError(t, err)
	}
	sort.Strings(ids)
	require.Equal(t, []string{
		"imgjob_restart_pending",
		"imgjob_restart_running",
	}, ids)
}

func durableImageJobTestJob(id string, status string, now time.Time) *openAIImageJob {
	return &openAIImageJob{
		ID:        id,
		Status:    status,
		Endpoint:  "/v1/images/generations",
		UserID:    11,
		APIKeyID:  22,
		Model:     "gpt-image-2",
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func durableImageJobTestRequest() *openAIImageJobRequest {
	return &openAIImageJobRequest{
		Endpoint:    "/v1/images/generations",
		ContentType: "application/json",
		Header:      make(http.Header),
		Body:        []byte(`{"model":"gpt-image-2","prompt":"fake only"}`),
	}
}

func durableImageJobTestConfig(dataDir string, maxPending int, maxPendingBytes int64) *config.Config {
	return &config.Config{
		Pricing: config.PricingConfig{DataDir: dataDir},
		Gateway: config.GatewayConfig{AsyncImageQueue: config.AsyncImageQueueConfig{
			Enabled:           true,
			MaxPendingTasks:   maxPending,
			MaxPendingBytes:   maxPendingBytes,
			WorkerCeiling:     64,
			MaxAttempts:       8,
			RetryBaseSeconds:  1,
			RetryMaxSeconds:   60,
			LeaseTTLSeconds:   60,
			StaleAfterSeconds: 120,
		}},
	}
}
