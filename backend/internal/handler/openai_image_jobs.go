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
)

type openAIImageJobStore struct {
	rootDir       string
	retentionDays int
	mu            sync.RWMutex
	jobs          map[string]*openAIImageJob
	cancelFuncs   map[string]context.CancelFunc
}

type openAIImageJob struct {
	ID          string     `json:"job_id"`
	Status      string     `json:"status"`
	Endpoint    string     `json:"endpoint"`
	UserID      int64      `json:"user_id"`
	APIKeyID    int64      `json:"api_key_id"`
	GroupID     *int64     `json:"group_id,omitempty"`
	Model       string     `json:"model,omitempty"`
	ImageSize   string     `json:"image_size,omitempty"`
	Prompt      string     `json:"prompt,omitempty"`
	Error       string     `json:"error,omitempty"`
	ErrorType   string     `json:"error_type,omitempty"`
	Retryable   *bool      `json:"retryable,omitempty"`
	HTTPStatus  int        `json:"http_status,omitempty"`
	ContentType string     `json:"content_type,omitempty"`
	ResultBytes int64      `json:"result_bytes,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	DurationMs  *int64     `json:"duration_ms,omitempty"`
}

type openAIImageJobResult struct {
	statusCode  int
	contentType string
	body        []byte
	err         error
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

	body, contentType, err := h.imageJobStore.readResult(job.ID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"type":    "not_found_error",
				"message": "Image job result is not available",
			},
		})
		return
	}
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(http.StatusOK, contentType, body)
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
	asyncRelease, acquired := h.tryAcquireAsyncImageExecution()
	if !acquired {
		c.Header("Retry-After", "3")
		h.errorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "Too many asynchronous image tasks are already running")
		return
	}

	authHeader := c.GetHeader("Authorization")
	if strings.TrimSpace(authHeader) == "" {
		authHeader = "Bearer " + apiKey.Key
	}
	job := &openAIImageJob{
		ID:        "imgjob_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Status:    openAIImageJobStatusPending,
		Endpoint:  endpoint,
		UserID:    subject.UserID,
		APIKeyID:  apiKey.ID,
		GroupID:   apiKey.GroupID,
		Model:     parsed.Model,
		ImageSize: parsed.SizeTier,
		Prompt:    truncateImageJobPrompt(parsed.Prompt),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := h.imageJobStore.create(job); err != nil {
		asyncRelease()
		reqLog.Error("openai.image_job.create_failed", zap.Error(err))
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to create image job")
		return
	}

	header := copyImageJobRequestHeaders(c.Request.Header)
	header.Set("Authorization", authHeader)
	header.Set("Content-Type", c.GetHeader("Content-Type"))
	preserveImageJobForwardedBaseHeaders(c, header)
	header.Set("X-Sub2API-Image-Job-ID", job.ID)
	if requestID := strings.TrimSpace(c.GetHeader("X-Request-Id")); requestID != "" {
		header.Set("X-Request-Id", requestID)
	}

	h.startImageJob(job.ID, endpoint, body, header, asyncRelease)
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
	result := h.forwardImageJob(ctx, endpoint, body, header, timeout)
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
	if err := h.imageJobStore.saveResult(jobID, result.statusCode, result.contentType, result.body, finished); err != nil {
		if h.imageJobStore.isCanceled(jobID) {
			return
		}
		errorType, retryable := classifyOpenAIImageJobFailure(0, "", err)
		h.imageJobStore.markFailed(jobID, finished, 0, err.Error(), errorType, retryable)
		return
	}
	h.imageJobStore.cleanupExpired()
}

func (h *OpenAIGatewayHandler) forwardImageJob(ctx context.Context, endpoint string, body []byte, header http.Header, timeout time.Duration) openAIImageJobResult {
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

	limit := openAIImageJobResultReadLimit(h.cfg)
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if readErr != nil {
		return openAIImageJobResult{statusCode: resp.StatusCode, err: readErr}
	}
	if int64(len(respBody)) > limit {
		return openAIImageJobResult{statusCode: resp.StatusCode, err: fmt.Errorf("image job result exceeds %d bytes", limit)}
	}
	return openAIImageJobResult{
		statusCode:  resp.StatusCode,
		contentType: strings.TrimSpace(resp.Header.Get("Content-Type")),
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
	if err := os.MkdirAll(s.jobDir(job.ID), 0755); err != nil {
		return err
	}
	s.mu.Lock()
	s.jobs[job.ID] = cloneOpenAIImageJob(job)
	s.mu.Unlock()
	return s.writeMeta(job)
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
		job.StartedAt = &started
		job.UpdatedAt = started
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
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if changed {
		return cloneOpenAIImageJob(job), true, s.writeMeta(job)
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
	if s.isCanceled(jobID) {
		return context.Canceled
	}
	dir := s.jobDir(jobID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "result.json"), body, 0644); err != nil {
		return err
	}
	return s.update(jobID, func(job *openAIImageJob) {
		if openAIImageJobTerminal(job.Status) {
			return
		}
		job.Status = openAIImageJobStatusSuccess
		job.HTTPStatus = statusCode
		job.ContentType = contentType
		job.ResultBytes = int64(len(body))
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
}

func (s *openAIImageJobStore) readResult(jobID string) ([]byte, string, error) {
	job, ok := s.get(jobID)
	if !ok {
		return nil, "", fmt.Errorf("image job not found")
	}
	body, err := os.ReadFile(filepath.Join(s.jobDir(jobID), "result.json"))
	if err != nil {
		return nil, "", err
	}
	return body, job.ContentType, nil
}

func (s *openAIImageJobStore) update(jobID string, mutate func(*openAIImageJob)) error {
	if s == nil || !isSafeImageJobID(jobID) {
		return fmt.Errorf("invalid image job id")
	}
	s.mu.Lock()
	job := cloneOpenAIImageJob(s.jobs[jobID])
	if job == nil {
		var err error
		job, err = s.readMeta(jobID)
		if err != nil {
			s.mu.Unlock()
			return err
		}
	}
	mutate(job)
	s.jobs[jobID] = cloneOpenAIImageJob(job)
	s.mu.Unlock()
	return s.writeMeta(job)
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
	return writeFileAtomic(filepath.Join(s.jobDir(job.ID), "meta.json"), data, 0644)
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
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
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
