package service

import (
	"context"
	"errors"
	"time"
)

var ErrOpenAIImageJobQueueEmpty = errors.New("openai image job queue is empty")

type OpenAIImageJobReservation struct {
	JobID string
}

// OpenAIImageJobQueue is the persistence boundary used by the HTTP handler.
// Redis is an implementation detail in repository, so handlers can be tested
// without depending on infrastructure packages.
type OpenAIImageJobQueue interface {
	Enqueue(ctx context.Context, jobID string) (bool, error)
	Reserve(ctx context.Context, blockTimeout time.Duration) (OpenAIImageJobReservation, error)
	RequeueAfter(ctx context.Context, jobID string, delay time.Duration) error
	Ack(ctx context.Context, jobID string) error
	Heartbeat(ctx context.Context, jobID string) error
	MoveDueDelayedToReady(ctx context.Context, limit int) (int, error)
	RecoverStaleActive(ctx context.Context, staleAfter time.Duration, limit int) (int, error)
	Restore(ctx context.Context, jobID string) error
	Pause(ctx context.Context, ttl time.Duration) error
	Resume(ctx context.Context) error
	IsPaused(ctx context.Context) (bool, error)
	ClaimIdempotency(ctx context.Context, scope string, jobID string) (existing string, claimed bool, err error)
	ReleaseIdempotency(ctx context.Context, scope string, jobID string) error
}
