package handler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type fakeOpenAIImageJobQueue struct {
	mu          sync.Mutex
	ready       []string
	delayed     map[string]time.Time
	active      map[string]time.Time
	inflight    map[string]struct{}
	idempotency map[string]string
	paused      bool
}

func newFakeOpenAIImageJobQueue() *fakeOpenAIImageJobQueue {
	return &fakeOpenAIImageJobQueue{
		delayed:     make(map[string]time.Time),
		active:      make(map[string]time.Time),
		inflight:    make(map[string]struct{}),
		idempotency: make(map[string]string),
	}
}

func (q *fakeOpenAIImageJobQueue) Enqueue(_ context.Context, jobID string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.inflight[jobID]; exists {
		return false, nil
	}
	q.inflight[jobID] = struct{}{}
	q.ready = append(q.ready, jobID)
	return true, nil
}

func (q *fakeOpenAIImageJobQueue) Reserve(ctx context.Context, blockTimeout time.Duration) (service.OpenAIImageJobReservation, error) {
	deadline := time.Now().Add(blockTimeout)
	for {
		q.mu.Lock()
		if !q.paused && len(q.ready) > 0 {
			jobID := q.ready[0]
			q.ready = q.ready[1:]
			q.active[jobID] = time.Now()
			q.mu.Unlock()
			return service.OpenAIImageJobReservation{JobID: jobID}, nil
		}
		q.mu.Unlock()
		if blockTimeout <= 0 || time.Now().After(deadline) {
			return service.OpenAIImageJobReservation{}, service.ErrOpenAIImageJobQueueEmpty
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return service.OpenAIImageJobReservation{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (q *fakeOpenAIImageJobQueue) RequeueAfter(_ context.Context, jobID string, delay time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.active, jobID)
	delete(q.delayed, jobID)
	if delay <= 0 {
		q.ready = append(q.ready, jobID)
	} else {
		q.delayed[jobID] = time.Now().Add(delay)
	}
	return nil
}

func (q *fakeOpenAIImageJobQueue) Ack(_ context.Context, jobID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.removeReadyLocked(jobID)
	delete(q.active, jobID)
	delete(q.delayed, jobID)
	delete(q.inflight, jobID)
	return nil
}

func (q *fakeOpenAIImageJobQueue) Heartbeat(_ context.Context, jobID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.active[jobID]; exists {
		q.active[jobID] = time.Now()
	}
	return nil
}

func (q *fakeOpenAIImageJobQueue) MoveDueDelayedToReady(_ context.Context, limit int) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	now := time.Now()
	moved := 0
	for jobID, due := range q.delayed {
		if moved >= limit {
			break
		}
		if due.After(now) {
			continue
		}
		delete(q.delayed, jobID)
		q.ready = append(q.ready, jobID)
		moved++
	}
	return moved, nil
}

func (q *fakeOpenAIImageJobQueue) RecoverStaleActive(_ context.Context, staleAfter time.Duration, limit int) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	cutoff := time.Now().Add(-staleAfter)
	moved := 0
	for jobID, heartbeat := range q.active {
		if moved >= limit {
			break
		}
		if heartbeat.After(cutoff) {
			continue
		}
		delete(q.active, jobID)
		q.ready = append(q.ready, jobID)
		moved++
	}
	return moved, nil
}

func (q *fakeOpenAIImageJobQueue) Restore(_ context.Context, jobID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.removeReadyLocked(jobID)
	delete(q.active, jobID)
	delete(q.delayed, jobID)
	q.inflight[jobID] = struct{}{}
	q.ready = append(q.ready, jobID)
	return nil
}

func (q *fakeOpenAIImageJobQueue) Pause(_ context.Context, _ time.Duration) error {
	q.mu.Lock()
	q.paused = true
	q.mu.Unlock()
	return nil
}

func (q *fakeOpenAIImageJobQueue) Resume(_ context.Context) error {
	q.mu.Lock()
	q.paused = false
	q.mu.Unlock()
	return nil
}

func (q *fakeOpenAIImageJobQueue) IsPaused(_ context.Context) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused, nil
}

func (q *fakeOpenAIImageJobQueue) ClaimIdempotency(_ context.Context, scope string, jobID string) (string, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if existing := q.idempotency[scope]; existing != "" {
		return existing, false, nil
	}
	q.idempotency[scope] = jobID
	return jobID, true, nil
}

func (q *fakeOpenAIImageJobQueue) ReleaseIdempotency(_ context.Context, scope string, jobID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.idempotency[scope] == jobID {
		delete(q.idempotency, scope)
	}
	return nil
}

func (q *fakeOpenAIImageJobQueue) makeDelayedDue(jobID string) {
	q.mu.Lock()
	q.delayed[jobID] = time.Now().Add(-time.Second)
	q.mu.Unlock()
}

func (q *fakeOpenAIImageJobQueue) delayedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.delayed)
}

func (q *fakeOpenAIImageJobQueue) readyCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.ready)
}

func (q *fakeOpenAIImageJobQueue) activeCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.active)
}

func (q *fakeOpenAIImageJobQueue) removeReadyLocked(jobID string) {
	filtered := q.ready[:0]
	for _, candidate := range q.ready {
		if candidate != jobID {
			filtered = append(filtered, candidate)
		}
	}
	q.ready = filtered
}

func requireOpenAIImageJobQueueEmpty(t *testing.T, queue openAIImageJobQueue) {
	t.Helper()
	_, err := queue.Reserve(context.Background(), 0)
	if !errors.Is(err, errOpenAIImageJobQueueEmpty) {
		t.Fatalf("Reserve() error = %v, want %v", err, errOpenAIImageJobQueueEmpty)
	}
}
