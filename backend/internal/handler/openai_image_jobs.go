package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	openAIImageJobStatusPending  = "pending"
	openAIImageJobStatusRunning  = "running"
	openAIImageJobStatusSuccess  = "success"
	openAIImageJobStatusFailed   = "failed"
	openAIImageJobStatusCanceled = "canceled"

	openAIImageJobErrorUnknown        = "unknown"
	openAIImageJobErrorInvalidRequest = "invalid_request"
	openAIImageJobErrorQuotaExceeded  = "quota_exceeded"
	openAIImageJobErrorRateLimited    = "rate_limited"
	openAIImageJobErrorAccountAuth    = "account_auth"
	openAIImageJobErrorUpstreamModel  = "upstream_model_error"
	openAIImageJobErrorQueueTimeout   = "queue_timeout"
	openAIImageJobErrorJobTimeout     = "job_timeout"
	openAIImageJobErrorClientCanceled = "client_canceled"
	openAIImageJobErrorNetwork        = "network_error"
	openAIImageJobErrorInternal       = "internal_error"

	defaultOpenAIImageJobRetentionDays = 30
	defaultOpenAIImageJobTimeout       = 30 * time.Minute
	defaultOpenAIImageJobResultLimit   = config.DefaultUpstreamResponseReadMaxBytes
	openAIImageJobErrorBodyLimit       = int64(64 << 10)
)

type openAIImageJobStore struct {
	rootDir       string
	retentionDays int
	maxPending    int
	maxBytes      int64
	mu            sync.RWMutex
	jobs          map[string]*openAIImageJob
	cancelFuncs   map[string]context.CancelFunc
}

type openAIImageJob struct {
	ID            string     `json:"job_id"`
	Status        string     `json:"status"`
	Endpoint      string     `json:"endpoint"`
	UserID        int64      `json:"user_id"`
	APIKeyID      int64      `json:"api_key_id"`
	GroupID       *int64     `json:"group_id,omitempty"`
	Model         string     `json:"model,omitempty"`
	ImageSize     string     `json:"image_size,omitempty"`
	Prompt        string     `json:"prompt,omitempty"`
	Error         string     `json:"error,omitempty"`
	ErrorType     string     `json:"error_type,omitempty"`
	Retryable     *bool      `json:"retryable,omitempty"`
	HTTPStatus    int        `json:"http_status,omitempty"`
	ContentType   string     `json:"content_type,omitempty"`
	ResultBytes   int64      `json:"result_bytes,omitempty"`
	RequestHash   string     `json:"request_hash,omitempty"`
	Attempts      int        `json:"attempts,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	DurationMs    *int64     `json:"duration_ms,omitempty"`
}

// openAIImageJobRequest is the durable execution envelope stored beside
// meta.json. Authorization is deliberately excluded: workers reload the API
// key by APIKeyID immediately before execution so disabled/deleted keys cannot
// be replayed from disk.
type openAIImageJobRequest struct {
	Endpoint    string      `json:"endpoint"`
	ContentType string      `json:"content_type,omitempty"`
	Header      http.Header `json:"header,omitempty"`
	Body        []byte      `json:"-"`
}

var errOpenAIImageJobQueueCapacity = errors.New("openai image job durable queue capacity exceeded")

type openAIImageJobResult struct {
	statusCode   int
	contentType  string
	body         []byte
	resultStored bool
	resultBytes  int64
	err          error
}

func newOpenAIImageJobStore(cfg *config.Config) *openAIImageJobStore {
	dataDir := "./data"
	if cfg != nil && strings.TrimSpace(cfg.Pricing.DataDir) != "" {
		dataDir = strings.TrimSpace(cfg.Pricing.DataDir)
	}
	retentionDays := envInt("IMAGE_JOBS_RETENTION_DAYS", defaultOpenAIImageJobRetentionDays)
	if retentionDays < 0 {
		retentionDays = defaultOpenAIImageJobRetentionDays
	}
	return &openAIImageJobStore{
		rootDir:       filepath.Join(dataDir, "image_jobs"),
		retentionDays: retentionDays,
		maxPending:    asyncImageQueueMaxPending(cfg),
		maxBytes:      asyncImageQueueMaxPendingBytes(cfg),
		jobs:          make(map[string]*openAIImageJob),
		cancelFuncs:   make(map[string]context.CancelFunc),
	}
}

// CreateImageEditJob accepts the same multipart body as /v1/images/edits, then
// runs the original sync endpoint in the background so client disconnects do not
// lose a completed image.
func (h *OpenAIGatewayHandler) CreateImageEditJob(c *gin.Context) {
	h.createImageJob(c, "/v1/images/edits")
}

// CreateImageGenerationJob accepts the same JSON body as /v1/images/generations
// and stores the final sync response under a job id.
func (h *OpenAIGatewayHandler) CreateImageGenerationJob(c *gin.Context) {
	h.createImageJob(c, "/v1/images/generations")
}

func (h *OpenAIGatewayHandler) GetImageJob(c *gin.Context) {
	job, ok := h.lookupImageJob(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, h.imageJobPayload(c, job))
}

func (h *OpenAIGatewayHandler) GetImageJobResult(c *gin.Context) {
	job, ok := h.lookupImageJob(c)
	if !ok {
		return
	}
	if job.Status != openAIImageJobStatusSuccess {
		status := http.StatusAccepted
		if job.Status == openAIImageJobStatusFailed || job.Status == openAIImageJobStatusCanceled {
			status = http.StatusConflict
		}
		c.JSON(status, h.imageJobPayload(c, job))
		return
	}

	resultFile, resultBytes, contentType, err := h.imageJobStore.openResult(job.ID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"type":    "not_found_error",
				"message": "Image job result is not available",
			},
		})
		return
	}
	defer func() { _ = resultFile.Close() }()
	if contentType == "" {
		contentType = "application/json"
	}
	c.DataFromReader(http.StatusOK, resultBytes, contentType, resultFile, nil)
}

func (h *OpenAIGatewayHandler) CancelImageJob(c *gin.Context) {
	job, ok := h.lookupImageJob(c)
	if !ok {
		return
	}
	if h.imageJobStore == nil {
		h.imageJobStore = newOpenAIImageJobStore(h.cfg)
	}
	canceledJob, changed, err := h.imageJobStore.cancel(job.ID, time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"type":    "api_error",
				"message": "Failed to cancel image job",
			},
		})
		return
	}
	if canceledJob == nil {
		canceledJob = job
	}
	status := http.StatusOK
	if !changed && canceledJob.Status != openAIImageJobStatusCanceled {
		status = http.StatusConflict
	}
	c.JSON(status, h.imageJobPayload(c, canceledJob))
}

func (h *OpenAIGatewayHandler) ServeImageJobPublicFile(c *gin.Context) {
	if h.imageJobStore == nil {
		h.imageJobStore = newOpenAIImageJobStore(h.cfg)
	}
	rel := strings.TrimPrefix(c.Param("filepath"), "/")
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"type": "invalid_request_error", "message": "Invalid image file path"},
		})
		return
	}
	parts := strings.Split(rel, "/")
	if len(parts) < 3 || parts[0] != "image_jobs" || !isSafeImageJobID(parts[1]) || parts[2] != "public" || !isAllowedImageJobPublicFileExtension(rel) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{"type": "not_found_error", "message": "Image file not found"},
		})
		return
	}
	fullPath := filepath.Join(h.imageJobStore.dataDir(), filepath.FromSlash(rel))
	data, err := os.ReadFile(fullPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{"type": "not_found_error", "message": "Image file not found"},
		})
		return
	}
	mimeType := http.DetectContentType(data)
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/") {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{"type": "not_found_error", "message": "Image file not found"},
		})
		return
	}
	c.Header("Cache-Control", "public, max-age=86400, immutable")
	c.Data(http.StatusOK, mimeType, data)
}

func isAllowedImageJobPublicFileExtension(path string) bool {
	switch strings.ToLower(strings.TrimSpace(filepath.Ext(path))) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	default:
		return false
	}
}

func (h *OpenAIGatewayHandler) createImageJob(c *gin.Context, endpoint string) {
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.image_jobs",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.String("endpoint", endpoint),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}
	if h.imageJobStore == nil {
		h.imageJobStore = newOpenAIImageJobStore(h.cfg)
	}
	if h.cfg != nil && h.cfg.Gateway.AsyncImageQueue.Enabled && h.imageJobDispatcher == nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Durable image job queue is unavailable")
		return
	}

	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	parseCtx := cloneRequestContextForImageJob(c, endpoint, body)
	parsed, err := h.gatewayService.ParseOpenAIImagesRequest(parseCtx, body)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !service.GroupAllowsImageGeneration(apiKey.Group) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", service.ImageGenerationPermissionMessage())
		return
	}
	if decision := h.checkSecurityAudit(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIImages, parsed.Model, parsed.ModerationBody()); decision != nil && !decision.AllowNextStage {
		h.openAISecurityAuditError(c, decision)
		return
	}

	requestHash := openAIImageJobRequestHash(endpoint, c.GetHeader("Content-Type"), body)
	job := &openAIImageJob{
		ID:          "imgjob_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Status:      openAIImageJobStatusPending,
		Endpoint:    endpoint,
		UserID:      subject.UserID,
		APIKeyID:    apiKey.ID,
		GroupID:     apiKey.GroupID,
		Model:       parsed.Model,
		ImageSize:   parsed.SizeTier,
		Prompt:      truncateImageJobPrompt(parsed.Prompt),
		RequestHash: requestHash,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey != "" && h.imageJobDispatcher != nil {
		existing, claimed, claimErr := h.imageJobDispatcher.claimIdempotency(c.Request.Context(), apiKey.ID, endpoint, idempotencyKey, job.ID)
		if claimErr != nil {
			reqLog.Error("openai.image_job.idempotency_failed", zap.Error(claimErr))
			h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Image job queue is temporarily unavailable")
			return
		}
		if !claimed {
			existingJob, exists := h.imageJobStore.get(existing)
			if !exists {
				// A process may have died after claiming the Redis key but before the
				// atomic directory rename. Release only our matching stale mapping.
				_ = h.imageJobDispatcher.releaseIdempotency(c.Request.Context(), apiKey.ID, endpoint, idempotencyKey, existing)
				existing, claimed, claimErr = h.imageJobDispatcher.claimIdempotency(c.Request.Context(), apiKey.ID, endpoint, idempotencyKey, job.ID)
				if claimErr != nil || !claimed {
					h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Image job idempotency state is temporarily unavailable")
					return
				}
			} else {
				if existingJob.RequestHash != "" && existingJob.RequestHash != requestHash {
					h.errorResponse(c, http.StatusConflict, "invalid_request_error", "Idempotency-Key was already used with a different image request")
					return
				}
				c.JSON(http.StatusAccepted, h.imageJobPayload(c, existingJob))
				return
			}
		}
	}

	header := copyImageJobRequestHeaders(c.Request.Header)
	header.Del("Authorization")
	header.Del("Proxy-Authorization")
	header.Set("Content-Type", c.GetHeader("Content-Type"))
	preserveImageJobForwardedBaseHeaders(c, header)
	header.Set("X-Sub2API-Image-Job-ID", job.ID)
	if requestID := strings.TrimSpace(c.GetHeader("X-Request-Id")); requestID != "" {
		header.Set("X-Request-Id", requestID)
	}
	request := &openAIImageJobRequest{Endpoint: endpoint, ContentType: c.GetHeader("Content-Type"), Header: header, Body: body}
	if err := h.imageJobStore.createWithRequest(job, request); err != nil {
		if idempotencyKey != "" && h.imageJobDispatcher != nil {
			_ = h.imageJobDispatcher.releaseIdempotency(c.Request.Context(), apiKey.ID, endpoint, idempotencyKey, job.ID)
		}
		reqLog.Error("openai.image_job.create_failed", zap.Error(err))
		if errors.Is(err, errOpenAIImageJobQueueCapacity) {
			c.Header("Retry-After", "30")
			h.errorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "Asynchronous image task queue is full")
			return
		}
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to create image job")
		return
	}

	if h.imageJobDispatcher != nil {
		if err := h.imageJobDispatcher.Enqueue(c.Request.Context(), job.ID); err != nil {
			// The durable directory is the source of truth. A reconciler will
			// publish it after Redis recovers, so an already persisted job remains
			// accepted instead of becoming an ambiguous client-side failure.
			reqLog.Warn("openai.image_job.enqueue_deferred", zap.String("job_id", job.ID), zap.Error(err))
		}
	} else {
		// Compatibility fallback for explicitly disabled queues. Production uses
		// the dispatcher by default; this path keeps local/test setups functional.
		h.startImageJob(job.ID, endpoint, body, header, func() {})
	}
	c.JSON(http.StatusAccepted, h.imageJobPayload(c, job))
}

func (h *OpenAIGatewayHandler) startImageJob(jobID string, endpoint string, body []byte, header http.Header, release func()) {
	go func() {
		defer release()
		defer func() {
			if recovered := recover(); recovered != nil {
				finished := time.Now()
				h.imageJobStore.markFailed(jobID, finished, http.StatusInternalServerError, "Image job panicked", openAIImageJobErrorInternal, true)
				logger.L().With(zap.String("component", "handler.openai_gateway.image_jobs")).Error(
					"openai.image_job.panicked",
					zap.String("job_id", jobID),
					zap.Any("panic", recovered),
				)
			}
		}()
		h.runImageJob(jobID, endpoint, body, header)
	}()
}

func (h *OpenAIGatewayHandler) runImageJob(jobID string, endpoint string, body []byte, header http.Header) {
	if h == nil || h.imageJobStore == nil {
		return
	}
	timeout := openAIImageJobTimeout(h.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	if !h.imageJobStore.registerCancel(jobID, cancel) {
		cancel()
		return
	}
	defer h.imageJobStore.unregisterCancel(jobID)
	defer cancel()

	started := time.Now()
	if !h.imageJobStore.markRunning(jobID, started) {
		return
	}
	result := h.forwardImageJob(ctx, jobID, endpoint, body, header, timeout)
	finished := time.Now()
	if h.imageJobStore.isCanceled(jobID) {
		return
	}
	if result.err != nil {
		errorType, retryable := classifyOpenAIImageJobFailure(result.statusCode, "", result.err)
		h.imageJobStore.markFailed(jobID, finished, result.statusCode, result.err.Error(), errorType, retryable)
		logger.L().With(zap.String("component", "handler.openai_gateway.image_jobs")).Warn(
			"openai.image_job.failed",
			zap.String("job_id", jobID),
			zap.Int("status_code", result.statusCode),
			zap.String("error_type", errorType),
			zap.Bool("retryable", retryable),
			zap.Error(result.err),
		)
		return
	}
	if result.statusCode < 200 || result.statusCode >= 300 {
		msg := strings.TrimSpace(string(result.body))
		if msg == "" {
			msg = http.StatusText(result.statusCode)
		}
		errorType, retryable := classifyOpenAIImageJobFailure(result.statusCode, msg, nil)
		h.imageJobStore.markFailed(jobID, finished, result.statusCode, msg, errorType, retryable)
		return
	}
	var saveErr error
	if result.resultStored {
		saveErr = h.imageJobStore.finishStoredResult(jobID, result.statusCode, result.contentType, result.resultBytes, finished)
	} else {
		saveErr = h.imageJobStore.saveResult(jobID, result.statusCode, result.contentType, result.body, finished)
	}
	if saveErr != nil {
		if h.imageJobStore.isCanceled(jobID) {
			return
		}
		errorType, retryable := classifyOpenAIImageJobFailure(0, "", saveErr)
		h.imageJobStore.markFailed(jobID, finished, 0, saveErr.Error(), errorType, retryable)
		return
	}
	h.imageJobStore.cleanupExpired()
}

func (h *OpenAIGatewayHandler) forwardImageJob(ctx context.Context, jobID string, endpoint string, body []byte, header http.Header, timeout time.Duration) openAIImageJobResult {
	url := "http://127.0.0.1:" + strconv.Itoa(serverPortForImageJob(h.cfg)) + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return openAIImageJobResult{err: err}
	}
	req.ContentLength = int64(len(body))
	for key, values := range header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Del("Content-Length")

	client := &http.Client{Timeout: timeout + 15*time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return openAIImageJobResult{err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if h.imageJobStore == nil {
			return openAIImageJobResult{statusCode: resp.StatusCode, err: errors.New("image job store is not available")}
		}
		resultBytes, writeErr := h.imageJobStore.stageResult(jobID, resp.Body, openAIImageJobResultReadLimit(h.cfg))
		if writeErr != nil {
			return openAIImageJobResult{statusCode: resp.StatusCode, err: writeErr}
		}
		return openAIImageJobResult{
			statusCode:   resp.StatusCode,
			contentType:  contentType,
			resultStored: true,
			resultBytes:  resultBytes,
		}
	}

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, openAIImageJobErrorBodyLimit+1))
	if readErr != nil {
		return openAIImageJobResult{statusCode: resp.StatusCode, err: readErr}
	}
	if int64(len(respBody)) > openAIImageJobErrorBodyLimit {
		respBody = respBody[:openAIImageJobErrorBodyLimit]
	}
	return openAIImageJobResult{
		statusCode:  resp.StatusCode,
		contentType: contentType,
		body:        respBody,
	}
}

func (h *OpenAIGatewayHandler) lookupImageJob(c *gin.Context) (*openAIImageJob, bool) {
	if h.imageJobStore == nil {
		h.imageJobStore = newOpenAIImageJobStore(h.cfg)
	}
	jobID := strings.TrimSpace(c.Param("job_id"))
	job, ok := h.imageJobStore.get(jobID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"type":    "not_found_error",
				"message": "Image job not found",
			},
		})
		return nil, false
	}
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey.ID != job.APIKeyID {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"type":    "not_found_error",
				"message": "Image job not found",
			},
		})
		return nil, false
	}
	return job, true
}

func (h *OpenAIGatewayHandler) imageJobPayload(c *gin.Context, job *openAIImageJob) gin.H {
	payload := gin.H{
		"job_id":     job.ID,
		"status":     job.Status,
		"endpoint":   job.Endpoint,
		"created_at": job.CreatedAt,
		"updated_at": job.UpdatedAt,
		"status_url": "/v1/image-jobs/" + job.ID,
		"result_url": "/v1/image-jobs/" + job.ID + "/result",
		"cancel_url": "/v1/image-jobs/" + job.ID + "/cancel",
	}
	if job.Model != "" {
		payload["model"] = job.Model
	}
	if job.ImageSize != "" {
		payload["image_size"] = job.ImageSize
	}
	if job.StartedAt != nil {
		payload["started_at"] = job.StartedAt
	}
	if job.FinishedAt != nil {
		payload["finished_at"] = job.FinishedAt
	}
	if job.DurationMs != nil {
		payload["duration_ms"] = *job.DurationMs
	}
	if job.Error != "" {
		payload["error"] = job.Error
	}
	if job.ErrorType != "" {
		payload["error_type"] = job.ErrorType
	}
	if job.Retryable != nil {
		payload["retryable"] = *job.Retryable
	}
	if job.HTTPStatus > 0 {
		payload["http_status"] = job.HTTPStatus
	}
	if job.ResultBytes > 0 {
		payload["result_bytes"] = job.ResultBytes
	}
	if c != nil && c.Request != nil {
		base := strings.TrimRight(requestBaseURL(c), "/")
		if base != "" {
			payload["status_url"] = base + "/v1/image-jobs/" + job.ID
			payload["result_url"] = base + "/v1/image-jobs/" + job.ID + "/result"
			payload["cancel_url"] = base + "/v1/image-jobs/" + job.ID + "/cancel"
		}
	}
	return payload
}

func (s *openAIImageJobStore) create(job *openAIImageJob) error {
	if s == nil || job == nil || strings.TrimSpace(job.ID) == "" {
		return fmt.Errorf("invalid image job")
	}
	if err := os.MkdirAll(s.jobDir(job.ID), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.jobDir(job.ID), 0o700); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeMeta(job); err != nil {
		return err
	}
	s.jobs[job.ID] = cloneOpenAIImageJob(job)
	return nil
}

func (s *openAIImageJobStore) createWithRequest(job *openAIImageJob, request *openAIImageJobRequest) error {
	if s == nil || job == nil || request == nil || !isSafeImageJobID(job.ID) || len(request.Body) == 0 {
		return fmt.Errorf("invalid image job request")
	}
	request.Endpoint = strings.TrimSpace(request.Endpoint)
	if request.Endpoint != "/v1/images/generations" && request.Endpoint != "/v1/images/edits" {
		return fmt.Errorf("invalid image job endpoint")
	}
	request.Header = sanitizePersistedImageJobHeaders(request.Header)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkCapacityLocked(int64(len(request.Body))); err != nil {
		return err
	}
	if err := os.MkdirAll(s.rootDir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(s.jobDir(job.ID)); err == nil {
		return fmt.Errorf("image job already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	tmpDir, err := os.MkdirTemp(s.rootDir, ".tmp-"+job.ID+"-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		return err
	}
	requestMeta, err := json.MarshalIndent(struct {
		Endpoint    string      `json:"endpoint"`
		ContentType string      `json:"content_type,omitempty"`
		Header      http.Header `json:"header,omitempty"`
	}{Endpoint: request.Endpoint, ContentType: request.ContentType, Header: request.Header}, "", "  ")
	if err != nil {
		return err
	}
	jobMeta, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "request.bin"), request.Body, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "request.json"), requestMeta, 0o600); err != nil {
		return err
	}
	// meta.json is written into the temporary directory last. Renaming the
	// directory publishes all three files as one durable unit to readers.
	if err := os.WriteFile(filepath.Join(tmpDir, "meta.json"), jobMeta, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpDir, s.jobDir(job.ID)); err != nil {
		return err
	}
	committed = true
	s.jobs[job.ID] = cloneOpenAIImageJob(job)
	return nil
}

func (s *openAIImageJobStore) readRequest(jobID string) (*openAIImageJobRequest, error) {
	if s == nil || !isSafeImageJobID(jobID) {
		return nil, fmt.Errorf("invalid image job id")
	}
	metaBytes, err := os.ReadFile(filepath.Join(s.jobDir(jobID), "request.json"))
	if err != nil {
		return nil, err
	}
	var request openAIImageJobRequest
	if err := json.Unmarshal(metaBytes, &request); err != nil {
		return nil, err
	}
	request.Header = sanitizePersistedImageJobHeaders(request.Header)
	request.Body, err = os.ReadFile(filepath.Join(s.jobDir(jobID), "request.bin"))
	if err != nil {
		return nil, err
	}
	if len(request.Body) == 0 {
		return nil, fmt.Errorf("image job request body is empty")
	}
	return &request, nil
}

func (s *openAIImageJobStore) checkCapacityLocked(incomingBytes int64) error {
	if s.maxPending <= 0 && s.maxBytes <= 0 {
		return nil
	}
	entries, err := os.ReadDir(s.rootDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pending := 0
	var pendingBytes int64
	for _, entry := range entries {
		if !entry.IsDir() || !isSafeImageJobID(entry.Name()) {
			continue
		}
		job, readErr := s.readMeta(entry.Name())
		if readErr != nil || openAIImageJobTerminal(job.Status) {
			continue
		}
		pending++
		if info, statErr := os.Stat(filepath.Join(s.jobDir(entry.Name()), "request.bin")); statErr == nil {
			pendingBytes += info.Size()
		}
	}
	if s.maxPending > 0 && pending+1 > s.maxPending {
		return errOpenAIImageJobQueueCapacity
	}
	if s.maxBytes > 0 && (incomingBytes > s.maxBytes || pendingBytes > s.maxBytes-incomingBytes) {
		return errOpenAIImageJobQueueCapacity
	}
	return nil
}

func (s *openAIImageJobStore) listRecoverable() ([]*openAIImageJob, error) {
	return s.listNonTerminal(true)
}

func (s *openAIImageJobStore) listPending() ([]*openAIImageJob, error) {
	return s.listNonTerminal(false)
}

func (s *openAIImageJobStore) listNonTerminal(resetRunning bool) ([]*openAIImageJob, error) {
	if s == nil {
		return nil, nil
	}
	entries, err := os.ReadDir(s.rootDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	jobs := make([]*openAIImageJob, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !isSafeImageJobID(entry.Name()) {
			continue
		}
		job, readErr := s.readMeta(entry.Name())
		if readErr != nil || openAIImageJobTerminal(job.Status) {
			continue
		}
		if job.Status == openAIImageJobStatusRunning {
			if !resetRunning {
				continue
			}
			now := time.Now()
			job.Status = openAIImageJobStatusPending
			job.StartedAt = nil
			job.DurationMs = nil
			job.UpdatedAt = now
			if err := s.writeMeta(job); err != nil {
				return nil, err
			}
		} else if job.Status != openAIImageJobStatusPending {
			continue
		}
		s.mu.Lock()
		s.jobs[job.ID] = cloneOpenAIImageJob(job)
		s.mu.Unlock()
		jobs = append(jobs, cloneOpenAIImageJob(job))
	}
	return jobs, nil
}

func (s *openAIImageJobStore) get(jobID string) (*openAIImageJob, bool) {
	if s == nil {
		return nil, false
	}
	jobID = strings.TrimSpace(jobID)
	if !isSafeImageJobID(jobID) {
		return nil, false
	}
	s.mu.RLock()
	job := cloneOpenAIImageJob(s.jobs[jobID])
	s.mu.RUnlock()
	if job != nil {
		return job, true
	}
	job, err := s.readMeta(jobID)
	if err != nil {
		return nil, false
	}
	s.mu.Lock()
	s.jobs[jobID] = cloneOpenAIImageJob(job)
	s.mu.Unlock()
	return job, true
}

func (s *openAIImageJobStore) registerCancel(jobID string, cancel context.CancelFunc) bool {
	if s == nil || cancel == nil || !isSafeImageJobID(jobID) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job := cloneOpenAIImageJob(s.jobs[jobID])
	if job == nil {
		var err error
		job, err = s.readMeta(jobID)
		if err != nil {
			return false
		}
	}
	if openAIImageJobTerminal(job.Status) {
		return false
	}
	if s.cancelFuncs == nil {
		s.cancelFuncs = make(map[string]context.CancelFunc)
	}
	s.cancelFuncs[jobID] = cancel
	return true
}

func (s *openAIImageJobStore) unregisterCancel(jobID string) {
	if s == nil || !isSafeImageJobID(jobID) {
		return
	}
	s.mu.Lock()
	delete(s.cancelFuncs, jobID)
	s.mu.Unlock()
}

func (s *openAIImageJobStore) markRunning(jobID string, started time.Time) bool {
	changed := false
	_ = s.update(jobID, func(job *openAIImageJob) {
		if job.Status != openAIImageJobStatusPending {
			return
		}
		job.Status = openAIImageJobStatusRunning
		job.Attempts++
		job.NextAttemptAt = nil
		job.StartedAt = &started
		job.UpdatedAt = started
		changed = true
	})
	return changed
}

func (s *openAIImageJobStore) markPendingForRetry(jobID string, next time.Time, preserveAttempt bool) bool {
	changed := false
	_ = s.update(jobID, func(job *openAIImageJob) {
		if openAIImageJobTerminal(job.Status) {
			return
		}
		job.Status = openAIImageJobStatusPending
		job.NextAttemptAt = &next
		job.StartedAt = nil
		job.DurationMs = nil
		job.UpdatedAt = time.Now()
		if !preserveAttempt && job.Attempts > 0 {
			job.Attempts--
		}
		changed = true
	})
	return changed
}

func (s *openAIImageJobStore) markFailed(jobID string, finished time.Time, statusCode int, message string, errorType string, retryable bool) bool {
	changed := false
	_ = s.update(jobID, func(job *openAIImageJob) {
		if openAIImageJobTerminal(job.Status) {
			return
		}
		job.Status = openAIImageJobStatusFailed
		job.Error = truncateImageJobError(message)
		job.ErrorType = normalizeOpenAIImageJobErrorType(errorType)
		job.Retryable = openAIImageJobBoolPtr(retryable)
		job.HTTPStatus = statusCode
		job.FinishedAt = &finished
		job.UpdatedAt = finished
		if job.StartedAt != nil {
			duration := finished.Sub(*job.StartedAt).Milliseconds()
			job.DurationMs = &duration
		}
		changed = true
	})
	if changed {
		s.deleteRequest(jobID)
	}
	return changed
}

func (s *openAIImageJobStore) cancel(jobID string, canceledAt time.Time) (*openAIImageJob, bool, error) {
	if s == nil || !isSafeImageJobID(jobID) {
		return nil, false, fmt.Errorf("invalid image job id")
	}
	var cancel context.CancelFunc
	var job *openAIImageJob
	changed := false
	s.mu.Lock()
	job = cloneOpenAIImageJob(s.jobs[jobID])
	if job == nil {
		var err error
		job, err = s.readMeta(jobID)
		if err != nil {
			s.mu.Unlock()
			return nil, false, err
		}
	}
	switch job.Status {
	case openAIImageJobStatusSuccess, openAIImageJobStatusFailed:
		s.mu.Unlock()
		return job, false, nil
	case openAIImageJobStatusCanceled:
		s.mu.Unlock()
		return job, true, nil
	default:
		job.Status = openAIImageJobStatusCanceled
		job.Error = "Image job canceled"
		job.ErrorType = openAIImageJobErrorClientCanceled
		job.Retryable = openAIImageJobBoolPtr(false)
		job.FinishedAt = &canceledAt
		job.UpdatedAt = canceledAt
		if job.StartedAt != nil {
			duration := canceledAt.Sub(*job.StartedAt).Milliseconds()
			job.DurationMs = &duration
		}
		if s.cancelFuncs != nil {
			cancel = s.cancelFuncs[jobID]
			delete(s.cancelFuncs, jobID)
		}
		s.jobs[jobID] = cloneOpenAIImageJob(job)
		changed = true
	}
	var persistErr error
	if changed {
		persistErr = s.writeMeta(job)
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if changed {
		if persistErr == nil {
			s.deleteRequest(jobID)
			s.deleteResult(jobID)
		}
		return cloneOpenAIImageJob(job), true, persistErr
	}
	return cloneOpenAIImageJob(job), false, nil
}

func (s *openAIImageJobStore) isCanceled(jobID string) bool {
	job, ok := s.get(jobID)
	return ok && job.Status == openAIImageJobStatusCanceled
}

func (s *openAIImageJobStore) saveResult(jobID string, statusCode int, contentType string, body []byte, finished time.Time) error {
	if s == nil {
		return fmt.Errorf("image job store is not available")
	}
	resultBytes, err := s.stageResult(jobID, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return err
	}
	return s.finishStoredResult(jobID, statusCode, contentType, resultBytes, finished)
}

// stageResult copies a successful upstream response directly to a private
// temporary file. It deliberately never materializes the complete response in
// memory. Sync + close happen before the atomic rename publishes result.json.
func (s *openAIImageJobStore) stageResult(jobID string, source io.Reader, limit int64) (resultBytes int64, retErr error) {
	if s == nil || source == nil || !isSafeImageJobID(jobID) {
		return 0, fmt.Errorf("invalid image job result")
	}
	if limit < 0 {
		return 0, fmt.Errorf("invalid image job result limit")
	}
	if s.isCanceled(jobID) {
		return 0, context.Canceled
	}
	dir := s.jobDir(jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".result-*.tmp")
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	committed := false
	closed := false
	defer func() {
		if !closed {
			if closeErr := tmp.Close(); retErr == nil && closeErr != nil {
				retErr = closeErr
			}
		}
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return 0, err
	}
	written, err := io.Copy(tmp, io.LimitReader(source, limit+1))
	if err != nil {
		return 0, err
	}
	if written > limit {
		return 0, fmt.Errorf("image job result exceeds %d bytes", limit)
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	closed = true
	if s.isCanceled(jobID) {
		return 0, context.Canceled
	}
	resultPath := filepath.Join(dir, "result.json")
	if err := os.Rename(tmpPath, resultPath); err != nil {
		return 0, err
	}
	committed = true
	if dirHandle, openErr := os.Open(dir); openErr == nil {
		syncErr := dirHandle.Sync()
		closeErr := dirHandle.Close()
		if syncErr != nil {
			_ = os.Remove(resultPath)
			return 0, syncErr
		}
		if closeErr != nil {
			_ = os.Remove(resultPath)
			return 0, closeErr
		}
	}
	if s.isCanceled(jobID) {
		_ = os.Remove(resultPath)
		return 0, context.Canceled
	}
	return written, nil
}

func (s *openAIImageJobStore) finishStoredResult(jobID string, statusCode int, contentType string, resultBytes int64, finished time.Time) error {
	if s == nil || !isSafeImageJobID(jobID) || resultBytes < 0 {
		return fmt.Errorf("invalid image job result")
	}
	committed := false
	err := s.update(jobID, func(job *openAIImageJob) {
		if openAIImageJobTerminal(job.Status) {
			return
		}
		committed = true
		job.Status = openAIImageJobStatusSuccess
		job.HTTPStatus = statusCode
		job.ContentType = contentType
		job.ResultBytes = resultBytes
		job.FinishedAt = &finished
		job.UpdatedAt = finished
		job.Error = ""
		job.ErrorType = ""
		job.Retryable = nil
		if job.StartedAt != nil {
			duration := finished.Sub(*job.StartedAt).Milliseconds()
			job.DurationMs = &duration
		}
	})
	if err != nil {
		return err
	}
	if !committed {
		s.deleteResult(jobID)
		return context.Canceled
	}
	s.deleteRequest(jobID)
	return nil
}

func (s *openAIImageJobStore) deleteRequest(jobID string) {
	if s == nil || !isSafeImageJobID(jobID) {
		return
	}
	_ = os.Remove(filepath.Join(s.jobDir(jobID), "request.bin"))
	_ = os.Remove(filepath.Join(s.jobDir(jobID), "request.json"))
}

func (s *openAIImageJobStore) deleteResult(jobID string) {
	if s == nil || !isSafeImageJobID(jobID) {
		return
	}
	_ = os.Remove(filepath.Join(s.jobDir(jobID), "result.json"))
}

func (s *openAIImageJobStore) openResult(jobID string) (*os.File, int64, string, error) {
	job, ok := s.get(jobID)
	if !ok {
		return nil, 0, "", fmt.Errorf("image job not found")
	}
	file, err := os.Open(filepath.Join(s.jobDir(jobID), "result.json"))
	if err != nil {
		return nil, 0, "", err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, "", err
	}
	return file, info.Size(), job.ContentType, nil
}

func (s *openAIImageJobStore) readResult(jobID string) ([]byte, string, error) {
	file, _, contentType, err := s.openResult(jobID)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(file)
	return body, contentType, err
}

func (s *openAIImageJobStore) update(jobID string, mutate func(*openAIImageJob)) error {
	if s == nil || !isSafeImageJobID(jobID) {
		return fmt.Errorf("invalid image job id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job := cloneOpenAIImageJob(s.jobs[jobID])
	if job == nil {
		var err error
		job, err = s.readMeta(jobID)
		if err != nil {
			return err
		}
	}
	mutate(job)
	if err := s.writeMeta(job); err != nil {
		return err
	}
	s.jobs[jobID] = cloneOpenAIImageJob(job)
	return nil
}

func (s *openAIImageJobStore) readMeta(jobID string) (*openAIImageJob, error) {
	if !isSafeImageJobID(jobID) {
		return nil, fmt.Errorf("invalid image job id")
	}
	data, err := os.ReadFile(filepath.Join(s.jobDir(jobID), "meta.json"))
	if err != nil {
		return nil, err
	}
	var job openAIImageJob
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

func classifyOpenAIImageJobFailure(statusCode int, message string, err error) (string, bool) {
	if err != nil {
		// Retrying after the upstream has already produced a successful image can
		// bill the same request again. A full filesystem cannot become healthy by
		// immediately repeating that upstream work, so surface it as permanent.
		if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
			return openAIImageJobErrorInternal, false
		}
		if errors.Is(err, context.Canceled) {
			return openAIImageJobErrorClientCanceled, false
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return openAIImageJobErrorJobTimeout, true
		}
		errText := strings.ToLower(err.Error())
		switch {
		case strings.Contains(errText, "context deadline exceeded"),
			strings.Contains(errText, "timeout"),
			strings.Contains(errText, "timed out"):
			return openAIImageJobErrorJobTimeout, true
		case strings.Contains(errText, "connection reset"),
			strings.Contains(errText, "connection refused"),
			strings.Contains(errText, "eof"),
			strings.Contains(errText, "empty reply"),
			strings.Contains(errText, "broken pipe"),
			strings.Contains(errText, "no such host"):
			return openAIImageJobErrorNetwork, true
		default:
			return openAIImageJobErrorInternal, true
		}
	}

	text := strings.ToLower(strings.TrimSpace(message))
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden ||
		strings.Contains(text, "unauthorized") || strings.Contains(text, "invalid api key") ||
		strings.Contains(text, "authentication") || strings.Contains(text, "forbidden") {
		return openAIImageJobErrorAccountAuth, false
	}
	if statusCode == http.StatusTooManyRequests || strings.Contains(text, "rate_limit") || strings.Contains(text, "rate limit") || strings.Contains(text, "too many") {
		if strings.Contains(text, "quota") || strings.Contains(text, "usage limit") || strings.Contains(text, "limit reached") || strings.Contains(text, "insufficient_quota") || strings.Contains(text, "credits") {
			return openAIImageJobErrorQuotaExceeded, false
		}
		return openAIImageJobErrorRateLimited, true
	}
	if strings.Contains(text, "quota") || strings.Contains(text, "usage limit") || strings.Contains(text, "insufficient_quota") || strings.Contains(text, "credits") {
		return openAIImageJobErrorQuotaExceeded, false
	}
	if statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout ||
		strings.Contains(text, "queue timeout") || strings.Contains(text, "queued timeout") {
		return openAIImageJobErrorQueueTimeout, true
	}
	if statusCode >= 400 && statusCode < 500 {
		return openAIImageJobErrorInvalidRequest, false
	}
	if statusCode >= 500 {
		return openAIImageJobErrorUpstreamModel, true
	}
	return openAIImageJobErrorUnknown, true
}

func normalizeOpenAIImageJobErrorType(errorType string) string {
	switch strings.TrimSpace(errorType) {
	case openAIImageJobErrorInvalidRequest,
		openAIImageJobErrorQuotaExceeded,
		openAIImageJobErrorRateLimited,
		openAIImageJobErrorAccountAuth,
		openAIImageJobErrorUpstreamModel,
		openAIImageJobErrorQueueTimeout,
		openAIImageJobErrorJobTimeout,
		openAIImageJobErrorClientCanceled,
		openAIImageJobErrorNetwork,
		openAIImageJobErrorInternal:
		return strings.TrimSpace(errorType)
	default:
		return openAIImageJobErrorUnknown
	}
}

func openAIImageJobTerminal(status string) bool {
	switch status {
	case openAIImageJobStatusSuccess, openAIImageJobStatusFailed, openAIImageJobStatusCanceled:
		return true
	default:
		return false
	}
}

func (s *openAIImageJobStore) writeMeta(job *openAIImageJob) error {
	if s == nil || job == nil || !isSafeImageJobID(job.ID) {
		return fmt.Errorf("invalid image job")
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.jobDir(job.ID), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.jobDir(job.ID), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.jobDir(job.ID), "meta.json"), data, 0o600)
}

func (s *openAIImageJobStore) cleanupExpired() {
	if s == nil || s.retentionDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -s.retentionDays)
	entries, err := os.ReadDir(s.rootDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isSafeImageJobID(entry.Name()) {
			continue
		}
		job, err := s.readMeta(entry.Name())
		if err != nil || !openAIImageJobTerminal(job.Status) || job.FinishedAt == nil || job.FinishedAt.After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(s.rootDir, entry.Name()))
		s.mu.Lock()
		delete(s.jobs, entry.Name())
		s.mu.Unlock()
	}
}

func (s *openAIImageJobStore) jobDir(jobID string) string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.rootDir, jobID)
}

func (s *openAIImageJobStore) dataDir() string {
	if s == nil {
		return "./data"
	}
	return filepath.Dir(s.rootDir)
}

func cloneRequestContextForImageJob(c *gin.Context, endpoint string, body []byte) *gin.Context {
	cloned := c.Copy()
	req := c.Request.Clone(c.Request.Context())
	req.URL.Path = endpoint
	req.RequestURI = endpoint
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	cloned.Request = req
	return cloned
}

func copyImageJobRequestHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for key, values := range src {
		switch strings.ToLower(key) {
		case "host", "content-length", "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
			continue
		default:
			for _, value := range values {
				dst.Add(key, value)
			}
		}
	}
	return dst
}

func sanitizePersistedImageJobHeaders(src http.Header) http.Header {
	dst := make(http.Header)
	for key, values := range src {
		switch http.CanonicalHeaderKey(key) {
		case "Accept", "Content-Type", "User-Agent", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Cf-Connecting-Ip", "X-Request-Id", "X-Image-Result-Delivery", "X-Sub2api-Image-Job-Id":
			for _, value := range values {
				if strings.TrimSpace(value) != "" {
					dst.Add(key, value)
				}
			}
		}
	}
	dst.Del("Authorization")
	dst.Del("Proxy-Authorization")
	dst.Del("Cookie")
	dst.Del("Set-Cookie")
	return dst
}

func preserveImageJobForwardedBaseHeaders(c *gin.Context, header http.Header) {
	if c == nil || c.Request == nil || header == nil {
		return
	}
	if strings.TrimSpace(header.Get("X-Forwarded-Host")) == "" && strings.TrimSpace(c.Request.Host) != "" {
		header.Set("X-Forwarded-Host", strings.TrimSpace(c.Request.Host))
	}
	if strings.TrimSpace(header.Get("X-Forwarded-Proto")) == "" {
		if c.Request.TLS != nil {
			header.Set("X-Forwarded-Proto", "https")
		} else {
			header.Set("X-Forwarded-Proto", "http")
		}
	}
}

func requestBaseURL(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	proto := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto"))
	if proto == "" {
		if c.Request.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := strings.TrimSpace(c.GetHeader("X-Forwarded-Host"))
	if host == "" {
		host = c.Request.Host
	}
	if host == "" {
		return ""
	}
	return proto + "://" + host
}

func openAIImageJobTimeout(cfg *config.Config) time.Duration {
	seconds := 0
	if cfg != nil {
		seconds = cfg.Gateway.ImageWorker.TimeoutSeconds
		if seconds <= 0 {
			seconds = cfg.Gateway.ResponseHeaderTimeout
		}
	}
	if seconds <= 0 {
		return defaultOpenAIImageJobTimeout
	}
	return time.Duration(seconds) * time.Second
}

func openAIImageJobResultReadLimit(cfg *config.Config) int64 {
	if cfg != nil && cfg.Gateway.UpstreamResponseReadMaxBytes > 0 {
		return cfg.Gateway.UpstreamResponseReadMaxBytes
	}
	return defaultOpenAIImageJobResultLimit
}

func serverPortForImageJob(cfg *config.Config) int {
	if cfg != nil && cfg.Server.Port > 0 {
		return cfg.Server.Port
	}
	port, err := strconv.Atoi(strings.TrimSpace(os.Getenv("SERVER_PORT")))
	if err == nil && port > 0 {
		return port
	}
	return 8080
}

func truncateImageJobPrompt(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	const limit = 512
	if len(prompt) <= limit {
		return prompt
	}
	return prompt[:limit] + "..."
}

func truncateImageJobError(message string) string {
	message = strings.TrimSpace(message)
	const limit = 2048
	if len(message) <= limit {
		return message
	}
	return message[:limit] + "..."
}

func isSafeImageJobID(jobID string) bool {
	if jobID == "" || len(jobID) > 80 {
		return false
	}
	for _, r := range jobID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func cloneOpenAIImageJob(job *openAIImageJob) *openAIImageJob {
	if job == nil {
		return nil
	}
	clone := *job
	if job.GroupID != nil {
		v := *job.GroupID
		clone.GroupID = &v
	}
	if job.StartedAt != nil {
		v := *job.StartedAt
		clone.StartedAt = &v
	}
	if job.NextAttemptAt != nil {
		v := *job.NextAttemptAt
		clone.NextAttemptAt = &v
	}
	if job.FinishedAt != nil {
		v := *job.FinishedAt
		clone.FinishedAt = &v
	}
	if job.DurationMs != nil {
		v := *job.DurationMs
		clone.DurationMs = &v
	}
	if job.Retryable != nil {
		v := *job.Retryable
		clone.Retryable = &v
	}
	return &clone
}

func openAIImageJobRequestHash(endpoint, contentType string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strings.TrimSpace(endpoint)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.TrimSpace(contentType)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func openAIImageJobIdempotencyScope(apiKeyID int64, endpoint, key string) string {
	return strconv.FormatInt(apiKeyID, 10) + "\x00" + strings.TrimSpace(endpoint) + "\x00" + strings.TrimSpace(key)
}

func asyncImageQueueMaxPending(cfg *config.Config) int {
	if cfg == nil || !cfg.Gateway.AsyncImageQueue.Enabled {
		return 0
	}
	return cfg.Gateway.AsyncImageQueue.MaxPendingTasks
}

func asyncImageQueueMaxPendingBytes(cfg *config.Config) int64 {
	if cfg == nil || !cfg.Gateway.AsyncImageQueue.Enabled {
		return 0
	}
	return cfg.Gateway.AsyncImageQueue.MaxPendingBytes
}

func openAIImageJobBoolPtr(v bool) *bool {
	return &v
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(path + strconv.FormatInt(time.Now().UnixNano(), 10)))
	tmp := path + "." + hex.EncodeToString(sum[:6]) + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
