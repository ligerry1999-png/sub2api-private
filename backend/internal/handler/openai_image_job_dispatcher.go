package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

type openAIImageJobDispatcherOptions struct {
	WorkerCeiling    int
	MaxAttempts      int
	RetryBase        time.Duration
	RetryMax         time.Duration
	LeaseTTL         time.Duration
	StaleAfter       time.Duration
	ReserveTimeout   time.Duration
	ReconcileEvery   time.Duration
	MaintenanceEvery time.Duration
}

type openAIImageJobDispatcher struct {
	h       *OpenAIGatewayHandler
	store   *openAIImageJobStore
	queue   openAIImageJobQueue
	opts    openAIImageJobDispatcherOptions
	execute func(context.Context, *openAIImageJob, *openAIImageJobRequest) openAIImageJobResult

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func newOpenAIImageJobDispatcher(h *OpenAIGatewayHandler, store *openAIImageJobStore, queue openAIImageJobQueue, cfg *config.Config) *openAIImageJobDispatcher {
	if h == nil || store == nil || queue == nil || cfg == nil || !cfg.Gateway.AsyncImageQueue.Enabled {
		return nil
	}
	qcfg := cfg.Gateway.AsyncImageQueue
	d := &openAIImageJobDispatcher{
		h:     h,
		store: store,
		queue: queue,
		opts: normalizeOpenAIImageJobDispatcherOptions(openAIImageJobDispatcherOptions{
			WorkerCeiling:  qcfg.WorkerCeiling,
			MaxAttempts:    qcfg.MaxAttempts,
			RetryBase:      time.Duration(qcfg.RetryBaseSeconds) * time.Second,
			RetryMax:       time.Duration(qcfg.RetryMaxSeconds) * time.Second,
			LeaseTTL:       time.Duration(qcfg.LeaseTTLSeconds) * time.Second,
			StaleAfter:     time.Duration(qcfg.StaleAfterSeconds) * time.Second,
			ReserveTimeout: time.Second,
			ReconcileEvery: 30 * time.Second,
		}),
	}
	d.execute = d.executeWithGateway
	return d
}

func normalizeOpenAIImageJobDispatcherOptions(opts openAIImageJobDispatcherOptions) openAIImageJobDispatcherOptions {
	if opts.WorkerCeiling <= 0 {
		opts.WorkerCeiling = 64
	}
	if opts.WorkerCeiling > 256 {
		opts.WorkerCeiling = 256
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 8
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = 3 * time.Second
	}
	if opts.RetryMax < opts.RetryBase {
		opts.RetryMax = time.Minute
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 35 * time.Minute
	}
	if opts.StaleAfter < opts.LeaseTTL {
		opts.StaleAfter = opts.LeaseTTL + 5*time.Minute
	}
	if opts.ReserveTimeout <= 0 {
		opts.ReserveTimeout = time.Second
	}
	if opts.ReconcileEvery <= 0 {
		opts.ReconcileEvery = 30 * time.Second
	}
	if opts.MaintenanceEvery <= 0 {
		opts.MaintenanceEvery = time.Second
	}
	return opts
}

func (d *openAIImageJobDispatcher) Start() {
	if d == nil || d.queue == nil || d.store == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d.cancel = cancel
	d.done = done
	// The disk directory is authoritative. Re-publish every non-terminal job
	// before workers start; Restore removes stale Redis active/delayed state so a
	// clean restart does not strand jobs for a full lease interval.
	if jobs, err := d.store.listRecoverable(); err == nil {
		for _, job := range jobs {
			if restorer, ok := d.queue.(interface {
				Restore(context.Context, string) error
			}); ok {
				_ = restorer.Restore(ctx, job.ID)
			} else {
				_, _ = d.queue.Enqueue(ctx, job.ID)
			}
		}
	} else {
		logger.L().Warn("openai.image_job.recovery_scan_failed", zap.Error(err))
	}

	var wg sync.WaitGroup
	for i := 0; i < d.opts.WorkerCeiling; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.runWorker(ctx)
		}()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		d.runMaintenance(ctx)
	}()
	go func() {
		defer wg.Done()
		d.runReconciler(ctx)
	}()
	go func() {
		wg.Wait()
		close(done)
	}()
}

func (d *openAIImageJobDispatcher) Stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	cancel, done := d.cancel, d.done
	d.cancel, d.done = nil, nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (d *openAIImageJobDispatcher) Enqueue(ctx context.Context, jobID string) error {
	if d == nil || d.queue == nil {
		return errors.New("openai image job dispatcher is unavailable")
	}
	_, err := d.queue.Enqueue(ctx, jobID)
	return err
}

func (d *openAIImageJobDispatcher) claimIdempotency(ctx context.Context, apiKeyID int64, endpoint, key, jobID string) (string, bool, error) {
	claimer, ok := d.queue.(interface {
		ClaimIdempotency(context.Context, string, string) (string, bool, error)
	})
	if !ok {
		return "", false, errors.New("image job idempotency is unavailable")
	}
	return claimer.ClaimIdempotency(ctx, openAIImageJobIdempotencyScope(apiKeyID, endpoint, key), jobID)
}

func (d *openAIImageJobDispatcher) releaseIdempotency(ctx context.Context, apiKeyID int64, endpoint, key, jobID string) error {
	releaser, ok := d.queue.(interface {
		ReleaseIdempotency(context.Context, string, string) error
	})
	if !ok {
		return nil
	}
	return releaser.ReleaseIdempotency(ctx, openAIImageJobIdempotencyScope(apiKeyID, endpoint, key), jobID)
}

func (d *openAIImageJobDispatcher) runWorker(ctx context.Context) {
	for ctx.Err() == nil {
		if err := d.RunOnce(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, errOpenAIImageJobQueueEmpty) {
			logger.L().Warn("openai.image_job.worker_iteration_failed", zap.Error(err))
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}
}

func (d *openAIImageJobDispatcher) RunOnce(ctx context.Context) error {
	if d == nil || d.queue == nil || d.store == nil || d.execute == nil {
		return nil
	}
	reserved, err := d.queue.Reserve(ctx, d.opts.ReserveTimeout)
	if err != nil {
		return err
	}
	job, ok := d.store.get(reserved.JobID)
	if !ok {
		return d.queue.Ack(context.WithoutCancel(ctx), reserved.JobID)
	}
	if openAIImageJobTerminal(job.Status) {
		return d.queue.Ack(context.WithoutCancel(ctx), job.ID)
	}
	if job.Status == openAIImageJobStatusRunning {
		return d.queue.RequeueAfter(context.WithoutCancel(ctx), job.ID, time.Second)
	}
	if job.NextAttemptAt != nil && time.Now().Before(*job.NextAttemptAt) {
		return d.queue.RequeueAfter(context.WithoutCancel(ctx), job.ID, time.Until(*job.NextAttemptAt))
	}
	request, err := d.store.readRequest(job.ID)
	if err != nil {
		d.store.markFailed(job.ID, time.Now(), http.StatusInternalServerError, "Image job request is not available", openAIImageJobErrorInternal, false)
		return d.queue.Ack(context.WithoutCancel(ctx), job.ID)
	}

	started := time.Now()
	if !d.store.markRunning(job.ID, started) {
		return d.queue.Ack(context.WithoutCancel(ctx), job.ID)
	}
	job, _ = d.store.get(job.ID)
	executionCtx, cancel := context.WithTimeout(ctx, openAIImageJobTimeout(d.h.cfg))
	if !d.store.registerCancel(job.ID, cancel) {
		cancel()
		return d.queue.Ack(context.WithoutCancel(ctx), job.ID)
	}
	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go d.runHeartbeat(executionCtx, job.ID, heartbeatStop, heartbeatDone)
	result := d.execute(executionCtx, job, request)
	close(heartbeatStop)
	<-heartbeatDone
	d.store.unregisterCancel(job.ID)
	cancel()

	if d.store.isCanceled(job.ID) {
		return d.queue.Ack(context.WithoutCancel(ctx), job.ID)
	}
	if ctx.Err() != nil {
		next := time.Now()
		d.store.markPendingForRetry(job.ID, next, false)
		return d.queue.RequeueAfter(context.Background(), job.ID, 0)
	}
	return d.finishAttempt(context.WithoutCancel(ctx), job, result)
}

func (d *openAIImageJobDispatcher) finishAttempt(ctx context.Context, job *openAIImageJob, result openAIImageJobResult) error {
	finished := time.Now()
	if result.err == nil && result.statusCode >= 200 && result.statusCode < 300 {
		var err error
		if result.resultStored {
			err = d.store.finishStoredResult(job.ID, result.statusCode, result.contentType, result.resultBytes, finished)
		} else {
			err = d.store.saveResult(job.ID, result.statusCode, result.contentType, result.body, finished)
		}
		if err == nil {
			return d.queue.Ack(ctx, job.ID)
		}
		result.err = err
	}

	message := strings.TrimSpace(string(result.body))
	errorType, retryable := classifyOpenAIImageJobFailure(result.statusCode, message, result.err)
	if result.err != nil {
		message = result.err.Error()
	}
	if message == "" {
		message = http.StatusText(result.statusCode)
	}
	localCapacity := isRetryableLocalImageJobCapacity(result.statusCode, message)
	if localCapacity {
		errorType = openAIImageJobErrorRateLimited
		retryable = true
	}
	attempts := job.Attempts
	if current, ok := d.store.get(job.ID); ok {
		attempts = current.Attempts
	}
	if retryable && (localCapacity || attempts < d.opts.MaxAttempts) {
		delay := d.retryDelay(attempts)
		next := time.Now().Add(delay)
		// Local user/account capacity is expected queueing pressure, not an
		// upstream attempt. Do not burn the retry budget for it.
		d.store.markPendingForRetry(job.ID, next, !localCapacity)
		return d.queue.RequeueAfter(ctx, job.ID, delay)
	}
	d.store.markFailed(job.ID, finished, result.statusCode, message, errorType, retryable)
	return d.queue.Ack(ctx, job.ID)
}

func (d *openAIImageJobDispatcher) executeWithGateway(ctx context.Context, job *openAIImageJob, request *openAIImageJobRequest) openAIImageJobResult {
	if d == nil || d.h == nil || d.h.apiKeyService == nil || job == nil || request == nil {
		return openAIImageJobResult{statusCode: http.StatusServiceUnavailable, err: errors.New("image gateway is unavailable")}
	}
	apiKey, err := d.h.apiKeyService.GetByID(ctx, job.APIKeyID)
	if err != nil || apiKey == nil || strings.TrimSpace(apiKey.Key) == "" {
		return openAIImageJobResult{statusCode: http.StatusUnauthorized, body: []byte("image job API key is no longer available")}
	}
	header := sanitizePersistedImageJobHeaders(request.Header)
	header.Set("Authorization", "Bearer "+apiKey.Key)
	header.Set("Content-Type", request.ContentType)
	header.Set("X-Sub2API-Image-Job-ID", job.ID)
	header.Set("Idempotency-Key", "image-job:"+job.ID)
	return d.h.forwardImageJob(ctx, job.ID, request.Endpoint, request.Body, header, openAIImageJobTimeout(d.h.cfg))
}

func (d *openAIImageJobDispatcher) runHeartbeat(ctx context.Context, jobID string, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	interval := d.opts.LeaseTTL / 3
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = d.queue.Heartbeat(context.WithoutCancel(ctx), jobID)
		}
	}
}

func (d *openAIImageJobDispatcher) runMaintenance(ctx context.Context) {
	ticker := time.NewTicker(d.opts.MaintenanceEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = d.queue.MoveDueDelayedToReady(ctx, 500)
			_, _ = d.queue.RecoverStaleActive(ctx, d.opts.StaleAfter, 500)
		}
	}
}

func (d *openAIImageJobDispatcher) runReconciler(ctx context.Context) {
	ticker := time.NewTicker(d.opts.ReconcileEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			jobs, err := d.store.listPending()
			if err != nil {
				continue
			}
			for _, job := range jobs {
				_, _ = d.queue.Enqueue(ctx, job.ID)
			}
		}
	}
}

func (d *openAIImageJobDispatcher) retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := d.opts.RetryBase
	for i := 1; i < attempt && delay < d.opts.RetryMax; i++ {
		delay *= 2
		if delay >= d.opts.RetryMax {
			return d.opts.RetryMax
		}
	}
	return delay
}

func isRetryableLocalImageJobCapacity(status int, message string) bool {
	if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
		return false
	}
	text := strings.ToLower(message)
	return strings.Contains(text, "concurrency") ||
		strings.Contains(text, "pending requests") ||
		strings.Contains(text, "no available compatible accounts") ||
		strings.Contains(text, "no available accounts")
}
