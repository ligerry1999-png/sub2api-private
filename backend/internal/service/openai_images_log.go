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
		Metadata: map[string]any{
			"size":            parsed.Size,
			"quality":         parsed.Quality,
			"background":      parsed.Background,
			"response_format": parsed.ResponseFormat,
			"n":               parsed.N,
			"stream":          parsed.Stream,
		},
	}
	if err := s.imageLogService.RecordOpenAIImages(ctx, input); err != nil {
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] image log record failed request_id=%s err=%v", requestID, err)
	}
}
