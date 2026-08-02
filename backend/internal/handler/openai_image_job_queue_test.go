package handler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRedisOpenAIImageJobQueueAccepts440UniqueJobsWithoutNetwork(t *testing.T) {
	queue, _, _ := newOpenAIImageJobQueueTest(t)
	ctx := context.Background()

	for i := 0; i < 440; i++ {
		accepted, err := queue.Enqueue(ctx, fmt.Sprintf("imgjob_bulk_%03d", i))
		require.NoError(t, err)
		require.True(t, accepted, "job %d should be durably admitted", i)
	}

	seen := make(map[string]struct{}, 440)
	for i := 0; i < 440; i++ {
		reserved, err := queue.Reserve(ctx, 0)
		require.NoError(t, err)
		seen[reserved.JobID] = struct{}{}
	}
	require.Len(t, seen, 440)
	_, err := queue.Reserve(ctx, 0)
	require.ErrorIs(t, err, errOpenAIImageJobQueueEmpty)
}

func TestRedisOpenAIImageJobQueueEnqueueIsIdempotent(t *testing.T) {
	queue, _, _ := newOpenAIImageJobQueueTest(t)
	ctx := context.Background()

	accepted, err := queue.Enqueue(ctx, "imgjob_idempotent")
	require.NoError(t, err)
	require.True(t, accepted)

	accepted, err = queue.Enqueue(ctx, "imgjob_idempotent")
	require.NoError(t, err)
	require.False(t, accepted)

	reserved, err := queue.Reserve(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, "imgjob_idempotent", reserved.JobID)
	_, err = queue.Reserve(ctx, 0)
	require.ErrorIs(t, err, errOpenAIImageJobQueueEmpty)
}

func TestRedisOpenAIImageJobQueueRequeuesTemporaryFailure(t *testing.T) {
	queue, _, _ := newOpenAIImageJobQueueTest(t)
	ctx := context.Background()
	accepted, err := queue.Enqueue(ctx, "imgjob_retry")
	require.NoError(t, err)
	require.True(t, accepted)
	reserved, err := queue.Reserve(ctx, 0)
	require.NoError(t, err)

	require.NoError(t, queue.RequeueAfter(ctx, reserved.JobID, 10*time.Millisecond))
	_, err = queue.Reserve(ctx, 0)
	require.ErrorIs(t, err, errOpenAIImageJobQueueEmpty)

	require.Eventually(t, func() bool {
		moved, moveErr := queue.MoveDueDelayedToReady(ctx, 10)
		return moveErr == nil && moved == 1
	}, time.Second, 10*time.Millisecond)

	retried, err := queue.Reserve(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, reserved.JobID, retried.JobID)
}

func TestRedisOpenAIImageJobQueueRecoversStaleActiveAfterRestart(t *testing.T) {
	queue, _, client := newOpenAIImageJobQueueTest(t)
	ctx := context.Background()
	accepted, err := queue.Enqueue(ctx, "imgjob_stale")
	require.NoError(t, err)
	require.True(t, accepted)
	reserved, err := queue.Reserve(ctx, 0)
	require.NoError(t, err)

	redisQueue, ok := queue.(*redisOpenAIImageJobQueue)
	require.True(t, ok)
	require.NoError(t, client.ZAdd(ctx, redisQueue.activeKey, redis.Z{
		Score:  float64(time.Now().Add(-time.Hour).UnixMilli()),
		Member: reserved.JobID,
	}).Err())

	recovered, err := queue.RecoverStaleActive(ctx, time.Minute, 10)
	require.NoError(t, err)
	require.Equal(t, 1, recovered)

	afterRestart, _, _ := newOpenAIImageJobQueueTestWithAddress(t, client.Options().Addr)
	retried, err := afterRestart.Reserve(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, reserved.JobID, retried.JobID)
}

func TestRedisOpenAIImageJobQueueAckAllowsFutureAdmission(t *testing.T) {
	queue, _, _ := newOpenAIImageJobQueueTest(t)
	ctx := context.Background()
	accepted, err := queue.Enqueue(ctx, "imgjob_ack")
	require.NoError(t, err)
	require.True(t, accepted)
	reserved, err := queue.Reserve(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, queue.Ack(ctx, reserved.JobID))

	accepted, err = queue.Enqueue(ctx, "imgjob_ack")
	require.NoError(t, err)
	require.True(t, accepted)
}

func newOpenAIImageJobQueueTest(t *testing.T) (openAIImageJobQueue, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	queue, _, client := newOpenAIImageJobQueueTestWithAddress(t, mr.Addr())
	return queue, mr, client
}

func newOpenAIImageJobQueueTestWithAddress(t *testing.T, address string) (openAIImageJobQueue, *config.Config, *redis.Client) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: address})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	cfg := &config.Config{Gateway: config.GatewayConfig{AsyncImageQueue: config.AsyncImageQueueConfig{
		Enabled:              true,
		MaxPendingTasks:      2000,
		MaxPendingBytes:      5 << 30,
		WorkerCeiling:        64,
		MaxAttempts:          8,
		RetryBaseSeconds:     1,
		RetryMaxSeconds:      60,
		LeaseTTLSeconds:      60,
		StaleAfterSeconds:    120,
		ReadyKey:             "test:image_jobs:ready",
		DelayedKey:           "test:image_jobs:delayed",
		ActiveKey:            "test:image_jobs:active",
		PauseKey:             "test:image_jobs:paused",
		InflightKeyPrefix:    "test:image_jobs:inflight:",
		IdempotencyKeyPrefix: "test:image_jobs:idem:",
	}}}
	queue := newRedisOpenAIImageJobQueue(client, cfg)
	require.NotNil(t, queue)
	return queue, cfg, client
}

func requireOpenAIImageJobQueueEmpty(t *testing.T, queue openAIImageJobQueue) {
	t.Helper()
	_, err := queue.Reserve(context.Background(), 0)
	require.True(t, errors.Is(err, errOpenAIImageJobQueueEmpty))
}
