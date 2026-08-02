package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/redis/go-redis/v9"
)

var errOpenAIImageJobQueueEmpty = errors.New("openai image job queue is empty")

type reservedOpenAIImageJob struct {
	JobID string
}

type openAIImageJobQueue interface {
	Enqueue(ctx context.Context, jobID string) (bool, error)
	Reserve(ctx context.Context, blockTimeout time.Duration) (reservedOpenAIImageJob, error)
	RequeueAfter(ctx context.Context, jobID string, delay time.Duration) error
	Ack(ctx context.Context, jobID string) error
	Heartbeat(ctx context.Context, jobID string) error
	MoveDueDelayedToReady(ctx context.Context, limit int) (int, error)
	RecoverStaleActive(ctx context.Context, staleAfter time.Duration, limit int) (int, error)
}

type redisOpenAIImageJobQueue struct {
	rdb               *redis.Client
	readyKey          string
	delayedKey        string
	activeKey         string
	pauseKey          string
	inflightPrefix    string
	idempotencyPrefix string
	inflightTTL       time.Duration
	idempotencyTTL    time.Duration
}

var openAIImageJobEnqueueScript = redis.NewScript(`
if redis.call("SET", KEYS[1], ARGV[1], "NX", "PX", ARGV[2]) then
  redis.call("LPUSH", KEYS[2], ARGV[1])
  return 1
end
return 0
`)

var openAIImageJobReserveScript = redis.NewScript(`
if redis.call("EXISTS", KEYS[3]) == 1 then
  return nil
end
local job = redis.call("RPOP", KEYS[1])
if not job then
  return nil
end
redis.call("ZADD", KEYS[2], ARGV[1], job)
return job
`)

var openAIImageJobMoveDelayedScript = redis.NewScript(`
local jobs = redis.call("ZRANGEBYSCORE", KEYS[1], "-inf", ARGV[1], "LIMIT", 0, ARGV[2])
for _, job in ipairs(jobs) do
  redis.call("ZREM", KEYS[1], job)
  redis.call("LPUSH", KEYS[2], job)
end
return #jobs
`)

var openAIImageJobRecoverStaleScript = redis.NewScript(`
local jobs = redis.call("ZRANGEBYSCORE", KEYS[1], "-inf", ARGV[1], "LIMIT", 0, ARGV[2])
for _, job in ipairs(jobs) do
  redis.call("ZREM", KEYS[1], job)
  redis.call("LPUSH", KEYS[2], job)
end
return #jobs
`)

var openAIImageJobRestoreScript = redis.NewScript(`
redis.call("LREM", KEYS[1], 0, ARGV[1])
redis.call("ZREM", KEYS[2], ARGV[1])
redis.call("ZREM", KEYS[3], ARGV[1])
redis.call("SET", KEYS[4], ARGV[1], "PX", ARGV[2])
redis.call("LPUSH", KEYS[1], ARGV[1])
return 1
`)

var openAIImageJobIdempotencyClaimScript = redis.NewScript(`
local existing = redis.call("GET", KEYS[1])
if existing then
  return {0, existing}
end
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[2])
return {1, ARGV[1]}
`)

var openAIImageJobIdempotencyReleaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func newRedisOpenAIImageJobQueue(rdb *redis.Client, cfg *config.Config) openAIImageJobQueue {
	if rdb == nil || cfg == nil || !cfg.Gateway.AsyncImageQueue.Enabled {
		return nil
	}
	qcfg := cfg.Gateway.AsyncImageQueue
	inflightTTL := time.Duration(qcfg.StaleAfterSeconds) * time.Second
	if inflightTTL < 7*24*time.Hour {
		inflightTTL = 7 * 24 * time.Hour
	}
	return &redisOpenAIImageJobQueue{
		rdb:               rdb,
		readyKey:          strings.TrimSpace(qcfg.ReadyKey),
		delayedKey:        strings.TrimSpace(qcfg.DelayedKey),
		activeKey:         strings.TrimSpace(qcfg.ActiveKey),
		pauseKey:          strings.TrimSpace(qcfg.PauseKey),
		inflightPrefix:    strings.TrimSpace(qcfg.InflightKeyPrefix),
		idempotencyPrefix: strings.TrimSpace(qcfg.IdempotencyKeyPrefix),
		inflightTTL:       inflightTTL,
		idempotencyTTL:    7 * 24 * time.Hour,
	}
}

func (q *redisOpenAIImageJobQueue) Enqueue(ctx context.Context, jobID string) (bool, error) {
	if q == nil || q.rdb == nil || !isSafeImageJobID(jobID) {
		return false, errors.New("invalid openai image job queue request")
	}
	applied, err := openAIImageJobEnqueueScript.Run(ctx, q.rdb,
		[]string{q.inflightPrefix + jobID, q.readyKey}, jobID, q.inflightTTL.Milliseconds()).Int()
	return applied == 1, err
}

func (q *redisOpenAIImageJobQueue) Reserve(ctx context.Context, blockTimeout time.Duration) (reservedOpenAIImageJob, error) {
	if q == nil || q.rdb == nil {
		return reservedOpenAIImageJob{}, errOpenAIImageJobQueueEmpty
	}
	deadline := time.Now().Add(blockTimeout)
	for {
		paused, pauseErr := q.IsPaused(ctx)
		if pauseErr != nil {
			return reservedOpenAIImageJob{}, pauseErr
		}
		if paused {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return reservedOpenAIImageJob{}, errOpenAIImageJobQueueEmpty
			}
			wait := time.Second
			if remaining < wait {
				wait = remaining
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return reservedOpenAIImageJob{}, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		raw, err := openAIImageJobReserveScript.Run(ctx, q.rdb,
			[]string{q.readyKey, q.activeKey, q.pauseKey}, time.Now().UnixMilli()).Result()
		if err == nil {
			jobID, ok := raw.(string)
			if ok && isSafeImageJobID(jobID) {
				return reservedOpenAIImageJob{JobID: jobID}, nil
			}
			return reservedOpenAIImageJob{}, errors.New("invalid openai image job queue payload")
		}
		if !errors.Is(err, redis.Nil) {
			return reservedOpenAIImageJob{}, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return reservedOpenAIImageJob{}, errOpenAIImageJobQueueEmpty
		}
		wait := time.Second
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return reservedOpenAIImageJob{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (q *redisOpenAIImageJobQueue) Pause(ctx context.Context, ttl time.Duration) error {
	if q == nil || q.rdb == nil {
		return errors.New("openai image job queue is unavailable")
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return q.rdb.Set(ctx, q.pauseKey, "1", ttl).Err()
}

func (q *redisOpenAIImageJobQueue) Resume(ctx context.Context) error {
	if q == nil || q.rdb == nil {
		return errors.New("openai image job queue is unavailable")
	}
	return q.rdb.Del(ctx, q.pauseKey).Err()
}

func (q *redisOpenAIImageJobQueue) IsPaused(ctx context.Context) (bool, error) {
	if q == nil || q.rdb == nil {
		return false, errors.New("openai image job queue is unavailable")
	}
	count, err := q.rdb.Exists(ctx, q.pauseKey).Result()
	return count > 0, err
}

func (q *redisOpenAIImageJobQueue) RequeueAfter(ctx context.Context, jobID string, delay time.Duration) error {
	if q == nil || q.rdb == nil || !isSafeImageJobID(jobID) {
		return errors.New("invalid openai image job queue request")
	}
	pipe := q.rdb.TxPipeline()
	pipe.ZRem(ctx, q.activeKey, jobID)
	pipe.ZRem(ctx, q.delayedKey, jobID)
	if delay <= 0 {
		pipe.LPush(ctx, q.readyKey, jobID)
	} else {
		pipe.ZAdd(ctx, q.delayedKey, redis.Z{Score: float64(time.Now().Add(delay).UnixMilli()), Member: jobID})
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (q *redisOpenAIImageJobQueue) Ack(ctx context.Context, jobID string) error {
	if q == nil || q.rdb == nil || !isSafeImageJobID(jobID) {
		return errors.New("invalid openai image job queue request")
	}
	pipe := q.rdb.TxPipeline()
	pipe.ZRem(ctx, q.activeKey, jobID)
	pipe.ZRem(ctx, q.delayedKey, jobID)
	pipe.Del(ctx, q.inflightPrefix+jobID)
	_, err := pipe.Exec(ctx)
	return err
}

func (q *redisOpenAIImageJobQueue) Heartbeat(ctx context.Context, jobID string) error {
	if q == nil || q.rdb == nil || !isSafeImageJobID(jobID) {
		return errors.New("invalid openai image job queue request")
	}
	return q.rdb.ZAddXX(ctx, q.activeKey, redis.Z{Score: float64(time.Now().UnixMilli()), Member: jobID}).Err()
}

func (q *redisOpenAIImageJobQueue) MoveDueDelayedToReady(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	return openAIImageJobMoveDelayedScript.Run(ctx, q.rdb,
		[]string{q.delayedKey, q.readyKey}, time.Now().UnixMilli(), limit).Int()
}

func (q *redisOpenAIImageJobQueue) RecoverStaleActive(ctx context.Context, staleAfter time.Duration, limit int) (int, error) {
	if staleAfter <= 0 {
		return 0, errors.New("invalid openai image job stale interval")
	}
	if limit <= 0 {
		limit = 100
	}
	cutoff := time.Now().Add(-staleAfter).UnixMilli()
	return openAIImageJobRecoverStaleScript.Run(ctx, q.rdb,
		[]string{q.activeKey, q.readyKey}, cutoff, limit).Int()
}

func (q *redisOpenAIImageJobQueue) Restore(ctx context.Context, jobID string) error {
	if q == nil || q.rdb == nil || !isSafeImageJobID(jobID) {
		return errors.New("invalid openai image job queue request")
	}
	return openAIImageJobRestoreScript.Run(ctx, q.rdb,
		[]string{q.readyKey, q.activeKey, q.delayedKey, q.inflightPrefix + jobID},
		jobID, q.inflightTTL.Milliseconds()).Err()
}

func (q *redisOpenAIImageJobQueue) ClaimIdempotency(ctx context.Context, scope string, jobID string) (existing string, claimed bool, err error) {
	if q == nil || q.rdb == nil || strings.TrimSpace(scope) == "" || !isSafeImageJobID(jobID) {
		return "", false, errors.New("invalid openai image job idempotency request")
	}
	result, err := openAIImageJobIdempotencyClaimScript.Run(ctx, q.rdb,
		[]string{q.idempotencyRedisKey(scope)}, jobID, q.idempotencyTTL.Milliseconds()).Result()
	if err != nil {
		return "", false, err
	}
	values, ok := result.([]interface{})
	if !ok || len(values) != 2 {
		return "", false, errors.New("invalid openai image job idempotency response")
	}
	flag, ok := values[0].(int64)
	if !ok {
		return "", false, errors.New("invalid openai image job idempotency response")
	}
	existing, ok = values[1].(string)
	if !ok || !isSafeImageJobID(existing) {
		return "", false, errors.New("invalid openai image job idempotency value")
	}
	return existing, flag == 1, nil
}

func (q *redisOpenAIImageJobQueue) ReleaseIdempotency(ctx context.Context, scope string, jobID string) error {
	if q == nil || q.rdb == nil || strings.TrimSpace(scope) == "" || !isSafeImageJobID(jobID) {
		return errors.New("invalid openai image job idempotency request")
	}
	return openAIImageJobIdempotencyReleaseScript.Run(ctx, q.rdb,
		[]string{q.idempotencyRedisKey(scope)}, jobID).Err()
}

func (q *redisOpenAIImageJobQueue) idempotencyRedisKey(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return q.idempotencyPrefix + hex.EncodeToString(sum[:])
}
