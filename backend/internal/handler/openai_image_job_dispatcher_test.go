package handler

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOpenAIImageJobDispatcherDoesNotExecuteCanceledPendingJob(t *testing.T) {
	store, queue, dispatcher, cleanup := newOpenAIImageJobDispatcherTest(t, 2)
	defer cleanup()
	job := enqueueFakeOpenAIImageJob(t, store, queue, "imgjob_cancel_pending")
	var executions atomic.Int64
	dispatcher.execute = func(context.Context, *openAIImageJob, *openAIImageJobRequest) openAIImageJobResult {
		executions.Add(1)
		return successfulFakeOpenAIImageJobResult()
	}

	canceled, changed, err := store.cancel(job.ID, time.Now())
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, openAIImageJobStatusCanceled, canceled.Status)
	require.NoError(t, dispatcher.RunOnce(context.Background()))
	require.Zero(t, executions.Load())
	requireOpenAIImageJobQueueEmpty(t, queue)
	_, err = store.readRequest(job.ID)
	require.Error(t, err, "canceling a queued job must remove its private request payload")
}

func TestOpenAIImageJobDispatcherRequeuesTransientFailures(t *testing.T) {
	tests := []struct {
		name         string
		result       openAIImageJobResult
		wantAttempts int
	}{
		{
			name:         "temporary upstream rate limit",
			result:       openAIImageJobResult{statusCode: http.StatusTooManyRequests, body: []byte("rate limit, retry later")},
			wantAttempts: 1,
		},
		{
			name:         "local user capacity does not burn retry budget",
			result:       openAIImageJobResult{statusCode: http.StatusTooManyRequests, body: []byte("user concurrency limit reached")},
			wantAttempts: 0,
		},
		{
			name:         "local account capacity does not burn retry budget",
			result:       openAIImageJobResult{statusCode: http.StatusServiceUnavailable, body: []byte("no available compatible accounts")},
			wantAttempts: 0,
		},
		{
			name:         "temporary upstream bad gateway",
			result:       openAIImageJobResult{statusCode: http.StatusBadGateway, body: []byte("temporary upstream failure")},
			wantAttempts: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, queue, dispatcher, cleanup := newOpenAIImageJobDispatcherTest(t, 2)
			defer cleanup()
			job := enqueueFakeOpenAIImageJob(t, store, queue, "imgjob_transient")
			dispatcher.opts.RetryBase = time.Hour
			dispatcher.opts.RetryMax = time.Hour
			dispatcher.execute = func(context.Context, *openAIImageJob, *openAIImageJobRequest) openAIImageJobResult {
				return tt.result
			}

			require.NoError(t, dispatcher.RunOnce(context.Background()))
			requeued, ok := store.get(job.ID)
			require.True(t, ok)
			require.Equal(t, openAIImageJobStatusPending, requeued.Status)
			require.Equal(t, tt.wantAttempts, requeued.Attempts)
			require.NotNil(t, requeued.NextAttemptAt)
			require.Empty(t, requeued.ErrorType, "transient failures must not be exposed as terminal errors")
			require.EqualValues(t, 1, queue.rdb.ZCard(context.Background(), queue.delayedKey).Val())

			// Make the retry due immediately, then prove the same durable job can finish.
			require.True(t, store.markPendingForRetry(job.ID, time.Now().Add(-time.Second), true))
			require.NoError(t, queue.rdb.ZAdd(context.Background(), queue.delayedKey, redis.Z{
				Score:  float64(time.Now().Add(-time.Second).UnixMilli()),
				Member: job.ID,
			}).Err())
			moved, err := queue.MoveDueDelayedToReady(context.Background(), 10)
			require.NoError(t, err)
			require.Equal(t, 1, moved)
			dispatcher.execute = func(context.Context, *openAIImageJob, *openAIImageJobRequest) openAIImageJobResult {
				return successfulFakeOpenAIImageJobResult()
			}
			require.NoError(t, dispatcher.RunOnce(context.Background()))
			finished, ok := store.get(job.ID)
			require.True(t, ok)
			require.Equal(t, openAIImageJobStatusSuccess, finished.Status)
			require.Empty(t, finished.ErrorType)
			requireOpenAIImageJobQueueEmpty(t, queue)
		})
	}
}

func TestOpenAIImageJobDispatcherFailsPermanent400WithoutRetry(t *testing.T) {
	store, queue, dispatcher, cleanup := newOpenAIImageJobDispatcherTest(t, 2)
	defer cleanup()
	job := enqueueFakeOpenAIImageJob(t, store, queue, "imgjob_permanent_400")
	dispatcher.execute = func(context.Context, *openAIImageJob, *openAIImageJobRequest) openAIImageJobResult {
		return openAIImageJobResult{statusCode: http.StatusBadRequest, body: []byte("invalid image size")}
	}

	require.NoError(t, dispatcher.RunOnce(context.Background()))
	failed, ok := store.get(job.ID)
	require.True(t, ok)
	require.Equal(t, openAIImageJobStatusFailed, failed.Status)
	require.Equal(t, openAIImageJobErrorInvalidRequest, failed.ErrorType)
	require.NotNil(t, failed.Retryable)
	require.False(t, *failed.Retryable)
	require.Equal(t, http.StatusBadRequest, failed.HTTPStatus)
	requireOpenAIImageJobQueueEmpty(t, queue)
	_, err := os.Stat(filepath.Join(store.jobDir(job.ID), "request.bin"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestOpenAIImageJobDispatcherPeakObeysSimulatedUserAndAccountSlots(t *testing.T) {
	const jobCount = 60
	tests := []struct {
		name         string
		userSlots    int
		accountSlots int
		wantPeak     int64
	}{
		{name: "account slots cap fifty executions", userSlots: 60, accountSlots: 50, wantPeak: 50},
		{name: "smaller user limit caps thirty executions", userSlots: 30, accountSlots: 50, wantPeak: 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, queue, dispatcher, cleanup := newOpenAIImageJobDispatcherTest(t, jobCount)
			defer cleanup()
			for i := 0; i < jobCount; i++ {
				enqueueFakeOpenAIImageJob(t, store, queue, "imgjob_slots_"+threeDigitNumber(i))
			}

			userGate := make(chan struct{}, tt.userSlots)
			accountGate := make(chan struct{}, tt.accountSlots)
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var active atomic.Int64
			var peak atomic.Int64
			dispatcher.execute = func(ctx context.Context, _ *openAIImageJob, _ *openAIImageJobRequest) openAIImageJobResult {
				select {
				case userGate <- struct{}{}:
				case <-ctx.Done():
					return openAIImageJobResult{err: ctx.Err()}
				}
				defer func() { <-userGate }()
				select {
				case accountGate <- struct{}{}:
				case <-ctx.Done():
					return openAIImageJobResult{err: ctx.Err()}
				}
				defer func() { <-accountGate }()

				current := active.Add(1)
				defer active.Add(-1)
				for {
					observed := peak.Load()
					if current <= observed || peak.CompareAndSwap(observed, current) {
						break
					}
				}
				select {
				case <-release:
					return successfulFakeOpenAIImageJobResult()
				case <-ctx.Done():
					return openAIImageJobResult{err: ctx.Err()}
				}
			}

			dispatcher.Start()
			defer dispatcher.Stop()
			require.Eventually(t, func() bool { return peak.Load() == tt.wantPeak }, 10*time.Second, 10*time.Millisecond)
			require.Equal(t, tt.wantPeak, peak.Load(), "execution peak must equal the smaller user/account slot pool")
			releaseOnce.Do(func() { close(release) })
			require.Eventually(t, func() bool {
				for i := 0; i < jobCount; i++ {
					job, ok := store.get("imgjob_slots_" + threeDigitNumber(i))
					if !ok || job.Status != openAIImageJobStatusSuccess {
						return false
					}
				}
				return true
			}, 15*time.Second, 10*time.Millisecond)
			require.Equal(t, tt.wantPeak, peak.Load())
		})
	}
}

func TestOpenAIImageJobDispatcherPauseLeavesReadyJobUntouchedUntilResume(t *testing.T) {
	store, queue, dispatcher, cleanup := newOpenAIImageJobDispatcherTest(t, 2)
	defer cleanup()
	job := enqueueFakeOpenAIImageJob(t, store, queue, "imgjob_pause_resume")
	var executions atomic.Int64
	dispatcher.execute = func(context.Context, *openAIImageJob, *openAIImageJobRequest) openAIImageJobResult {
		executions.Add(1)
		return successfulFakeOpenAIImageJobResult()
	}

	require.NoError(t, queue.Pause(context.Background(), time.Minute))
	err := dispatcher.RunOnce(context.Background())
	require.ErrorIs(t, err, errOpenAIImageJobQueueEmpty)
	require.Zero(t, executions.Load())
	pending, ok := store.get(job.ID)
	require.True(t, ok)
	require.Equal(t, openAIImageJobStatusPending, pending.Status)
	require.EqualValues(t, 1, queue.rdb.LLen(context.Background(), queue.readyKey).Val())
	require.EqualValues(t, 0, queue.rdb.ZCard(context.Background(), queue.activeKey).Val())

	require.NoError(t, queue.Resume(context.Background()))
	require.NoError(t, dispatcher.RunOnce(context.Background()))
	finished, ok := store.get(job.ID)
	require.True(t, ok)
	require.Equal(t, openAIImageJobStatusSuccess, finished.Status)
	require.Equal(t, int64(1), executions.Load())
	requireOpenAIImageJobQueueEmpty(t, queue)
}

func TestOpenAIImageJobDispatcherRestoresInterruptedJobAfterRestart(t *testing.T) {
	dataDir := t.TempDir()
	cfg := durableImageJobTestConfig(dataDir, 10, 1<<20)
	cfg.Gateway.AsyncImageQueue.WorkerCeiling = 1
	cfg.Gateway.AsyncImageQueue.ReadyKey = "test:restart:ready"
	cfg.Gateway.AsyncImageQueue.DelayedKey = "test:restart:delayed"
	cfg.Gateway.AsyncImageQueue.ActiveKey = "test:restart:active"
	cfg.Gateway.AsyncImageQueue.PauseKey = "test:restart:paused"
	cfg.Gateway.AsyncImageQueue.InflightKeyPrefix = "test:restart:inflight:"
	cfg.Gateway.AsyncImageQueue.IdempotencyKeyPrefix = "test:restart:idem:"
	beforeRestart := newOpenAIImageJobStore(cfg)
	now := time.Now()
	job := durableImageJobTestJob("imgjob_interrupted_restart", openAIImageJobStatusRunning, now)
	job.Attempts = 1
	require.NoError(t, beforeRestart.createWithRequest(job, durableImageJobTestRequest()))

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { require.NoError(t, client.Close()) }()
	queue, ok := newRedisOpenAIImageJobQueue(client, cfg).(*redisOpenAIImageJobQueue)
	require.True(t, ok)
	restartedStore := newOpenAIImageJobStore(cfg)
	h := &OpenAIGatewayHandler{cfg: cfg}
	dispatcher := newOpenAIImageJobDispatcher(h, restartedStore, queue, cfg)
	require.NotNil(t, dispatcher)
	dispatcher.opts.ReserveTimeout = 5 * time.Millisecond
	dispatcher.opts.MaintenanceEvery = 5 * time.Millisecond
	var executions atomic.Int64
	dispatcher.execute = func(context.Context, *openAIImageJob, *openAIImageJobRequest) openAIImageJobResult {
		executions.Add(1)
		return successfulFakeOpenAIImageJobResult()
	}

	dispatcher.Start()
	defer dispatcher.Stop()
	require.Eventually(t, func() bool {
		recovered, ok := restartedStore.get(job.ID)
		return ok && recovered.Status == openAIImageJobStatusSuccess
	}, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(1), executions.Load())
}

func newOpenAIImageJobDispatcherTest(t *testing.T, workers int) (*openAIImageJobStore, *redisOpenAIImageJobQueue, *openAIImageJobDispatcher, func()) {
	t.Helper()
	cfg := durableImageJobTestConfig(t.TempDir(), 100, 16<<20)
	cfg.Gateway.AsyncImageQueue.WorkerCeiling = workers
	cfg.Gateway.AsyncImageQueue.ReadyKey = "test:dispatcher:ready"
	cfg.Gateway.AsyncImageQueue.DelayedKey = "test:dispatcher:delayed"
	cfg.Gateway.AsyncImageQueue.ActiveKey = "test:dispatcher:active"
	cfg.Gateway.AsyncImageQueue.PauseKey = "test:dispatcher:paused"
	cfg.Gateway.AsyncImageQueue.InflightKeyPrefix = "test:dispatcher:inflight:"
	cfg.Gateway.AsyncImageQueue.IdempotencyKeyPrefix = "test:dispatcher:idem:"
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	queue, ok := newRedisOpenAIImageJobQueue(client, cfg).(*redisOpenAIImageJobQueue)
	require.True(t, ok)
	store := newOpenAIImageJobStore(cfg)
	h := &OpenAIGatewayHandler{cfg: cfg}
	dispatcher := newOpenAIImageJobDispatcher(h, store, queue, cfg)
	require.NotNil(t, dispatcher)
	dispatcher.opts.ReserveTimeout = 5 * time.Millisecond
	dispatcher.opts.MaintenanceEvery = 5 * time.Millisecond
	cleanup := func() {
		dispatcher.Stop()
		require.NoError(t, client.Close())
		mr.Close()
	}
	return store, queue, dispatcher, cleanup
}

func enqueueFakeOpenAIImageJob(t *testing.T, store *openAIImageJobStore, queue openAIImageJobQueue, jobID string) *openAIImageJob {
	t.Helper()
	job := durableImageJobTestJob(jobID, openAIImageJobStatusPending, time.Now())
	require.NoError(t, store.createWithRequest(job, durableImageJobTestRequest()))
	inserted, err := queue.Enqueue(context.Background(), job.ID)
	require.NoError(t, err)
	require.True(t, inserted)
	return job
}

func successfulFakeOpenAIImageJobResult() openAIImageJobResult {
	return openAIImageJobResult{
		statusCode:  http.StatusOK,
		contentType: "application/json",
		body:        []byte(`{"created":1,"data":[{"url":"https://example.invalid/fake.png"}]}`),
	}
}

func threeDigitNumber(value int) string {
	return string([]byte{
		byte('0' + value/100%10),
		byte('0' + value/10%10),
		byte('0' + value%10),
	})
}
