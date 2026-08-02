package handler

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAIImageJobStoreStagesLargeResultAsStream(t *testing.T) {
	store := newOpenAIImageJobStore(&config.Config{Pricing: config.PricingConfig{DataDir: t.TempDir()}})
	now := time.Now()
	job := &openAIImageJob{
		ID: "imgjob_stream_result", Status: openAIImageJobStatusRunning,
		Endpoint: "/v1/images/generations", UserID: 1, APIKeyID: 2,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, store.create(job))

	const resultSize = int64(8 << 20)
	reader := io.LimitReader(zeroReader{}, resultSize)
	written, err := store.stageResult(job.ID, reader, resultSize)
	require.NoError(t, err)
	require.Equal(t, resultSize, written)
	require.NoError(t, store.finishStoredResult(job.ID, http.StatusOK, "application/json", written, now.Add(time.Second)))

	info, err := os.Stat(filepath.Join(store.jobDir(job.ID), "result.json"))
	require.NoError(t, err)
	require.Equal(t, resultSize, info.Size())
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	completed, ok := store.get(job.ID)
	require.True(t, ok)
	require.Equal(t, openAIImageJobStatusSuccess, completed.Status)
	require.Equal(t, resultSize, completed.ResultBytes)
}

func TestOpenAIImageJobStoreRejectsOversizedStreamWithoutPublishing(t *testing.T) {
	store := newOpenAIImageJobStore(&config.Config{Pricing: config.PricingConfig{DataDir: t.TempDir()}})
	now := time.Now()
	job := &openAIImageJob{
		ID: "imgjob_stream_limit", Status: openAIImageJobStatusRunning,
		Endpoint: "/v1/images/generations", UserID: 1, APIKeyID: 2,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, store.create(job))

	_, err := store.stageResult(job.ID, bytes.NewReader(make([]byte, 1025)), 1024)
	require.ErrorContains(t, err, "exceeds 1024 bytes")
	_, statErr := os.Stat(filepath.Join(store.jobDir(job.ID), "result.json"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
	temps, globErr := filepath.Glob(filepath.Join(store.jobDir(job.ID), ".result-*.tmp"))
	require.NoError(t, globErr)
	require.Empty(t, temps)
}

func TestClassifyOpenAIImageJobDiskFullIsPermanent(t *testing.T) {
	for _, diskErr := range []error{syscall.ENOSPC, syscall.EDQUOT} {
		errorType, retryable := classifyOpenAIImageJobFailure(http.StatusOK, "", &os.PathError{Op: "write", Path: "result.json", Err: diskErr})
		require.Equal(t, openAIImageJobErrorInternal, errorType)
		require.False(t, retryable)
	}
}

// zeroReader is intentionally streaming-only: it generates bytes on demand
// and does not allocate a result-sized backing slice.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	clear(p)
	return len(p), nil
}
