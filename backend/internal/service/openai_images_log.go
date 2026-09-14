package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (s *OpenAIGatewayService) recordOpenAIImagesLog(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	parsed *OpenAIImagesRequest,
	requestModel string,
	source string,
	requestID string,
	startTime time.Time,
	results []openAIResponsesImageResult,
	extraMetadata map[string]any,
) {
	if s == nil || s.imageLogService == nil || c == nil || parsed == nil || len(results) == 0 {
		return
	}
	apiKeyValue, exists := c.Get("api_key")
	if !exists {
		return
	}
	apiKey, ok := apiKeyValue.(*APIKey)
	if !ok || apiKey == nil || apiKey.User == nil {
		return
	}

	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		requestID = uuid.NewString()
	}
	durationMs := int(time.Since(startTime).Milliseconds())
	if startTime.IsZero() {
		durationMs = 0
	}
	createdAt := time.Now()
	received := summarizeOpenAIImagesReceivedRequest(parsed)
	if submitEndpoint := safeImageJobSubmitEndpoint(c); submitEndpoint != "" {
		received["execution_endpoint"] = received["endpoint"]
		received["endpoint"] = submitEndpoint
	}
	effective := summarizeOpenAIImagesEffectiveRequest(parsed, requestModel)
	metadata := map[string]any{
		// Keep the flat fields for existing admin consumers and add structured
		// snapshots for comparing the gateway stages without storing the body.
		"size":            parsed.Size,
		"quality":         parsed.Quality,
		"background":      parsed.Background,
		"output_format":   parsed.OutputFormat,
		"response_format": parsed.ResponseFormat,
		"n":               parsed.N,
		"stream":          parsed.Stream,
		"received":         received,
		"effective":        effective,
		"result":           summarizeOpenAIImagesResults(results),
		"upstream": map[string]any{
			"route":        source,
			"model":        strings.TrimSpace(requestModel),
			"platform":     strings.TrimSpace(account.Platform),
			"account_type": strings.TrimSpace(account.Type),
			"account_id":   account.ID,
		},
	}
	if jobID := safeImageJobID(c); jobID != "" {
		metadata["image_job_id"] = jobID
	}
	input := &RecordImageLogInput{
		User:       apiKey.User,
		APIKey:     apiKey,
		Account:    account,
		RequestID:  requestID,
		Source:     source,
		Endpoint:   parsed.Endpoint,
		Model:      requestModel,
		Prompt:     parsed.Prompt,
		ImageSize:  parsed.SizeTier,
		DurationMs: durationMs,
		Results:    results,
		CreatedAt:  createdAt,
		Metadata: metadata,
	}
	for key, value := range extraMetadata {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		input.Metadata[key] = value
	}
	if err := s.imageLogService.RecordOpenAIImages(ctx, input); err != nil {
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] image log record failed request_id=%s err=%v", requestID, err)
	}
}
