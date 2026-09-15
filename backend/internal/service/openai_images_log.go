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
		"received":        received,
		"effective":       effective,
		"result":          summarizeOpenAIImagesResults(results),
	}
	if account != nil {
		metadata["upstream"] = map[string]any{
			"route":        source,
			"model":        strings.TrimSpace(requestModel),
			"platform":     strings.TrimSpace(account.Platform),
			"account_type": strings.TrimSpace(account.Type),
			"account_id":   account.ID,
		}
	}
	if jobID := safeImageJobID(c); jobID != "" {
		metadata["image_job_id"] = jobID
	}
	for key, value := range extraMetadata {
		if key = strings.TrimSpace(key); key != "" {
			metadata[key] = value
		}
	}
	duration := time.Duration(0)
	if !startTime.IsZero() {
		duration = time.Since(startTime)
	}
	s.recordImageGenerationLog(ctx, c, account, requestID, source, parsed.Endpoint, requestModel, parsed.Prompt, parsed.SizeTier, duration, results, metadata)
}

// RecordImageGenerationLog records a successful image result from any gateway
// route. The API key is read from the authenticated Gin context, so newly
// added channels/accounts/groups are included automatically without a static
// account allowlist.
func (s *OpenAIGatewayService) RecordImageGenerationLog(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	requestID string,
	source string,
	endpoint string,
	model string,
	prompt string,
	imageSize string,
	duration time.Duration,
	results []ImageLogResult,
	extraMetadata map[string]any,
) {
	s.recordImageGenerationLog(ctx, c, account, requestID, source, endpoint, model, prompt, imageSize, duration, results, extraMetadata)
}

func (s *OpenAIGatewayService) recordImageGenerationLog(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	requestID string,
	source string,
	endpoint string,
	model string,
	prompt string,
	imageSize string,
	duration time.Duration,
	results []ImageLogResult,
	extraMetadata map[string]any,
) {
	if s == nil || s.imageLogService == nil || c == nil || len(results) == 0 {
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
	results = s.resolveImageLogResults(ctx, account, results)
	if len(results) == 0 {
		return
	}
	if duration < 0 {
		duration = 0
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		requestID = uuid.NewString()
	}
	metadata := make(map[string]any, len(extraMetadata)+1)
	for key, value := range extraMetadata {
		if key = strings.TrimSpace(key); key != "" {
			metadata[key] = value
		}
	}
	if account != nil {
		metadata["upstream"] = map[string]any{
			"route":        strings.TrimSpace(source),
			"model":        strings.TrimSpace(model),
			"platform":     strings.TrimSpace(account.Platform),
			"account_type": strings.TrimSpace(account.Type),
			"account_id":   account.ID,
		}
	}
	input := &RecordImageLogInput{
		User:       apiKey.User,
		APIKey:     apiKey,
		Account:    account,
		RequestID:  requestID,
		Source:     source,
		Endpoint:   endpoint,
		Model:      model,
		Prompt:     prompt,
		ImageSize:  imageSize,
		DurationMs: int(duration.Milliseconds()),
		Results:    results,
		CreatedAt:  time.Now(),
		Metadata:   metadata,
	}
	if err := s.imageLogService.RecordOpenAIImages(ctx, input); err != nil {
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] image log record failed request_id=%s err=%v", requestID, err)
	}
}

// resolveImageLogResults turns provider image URLs into the base64 payload that
// ImageLogService stores. URL downloads use the existing outbound URL validator
// and public-host-only HTTP client; failed downloads are skipped instead of
// making an otherwise successful generation request fail.
func (s *OpenAIGatewayService) resolveImageLogResults(ctx context.Context, account *Account, results []ImageLogResult) []ImageLogResult {
	resolved := make([]ImageLogResult, 0, len(results))
	for _, result := range results {
		if strings.TrimSpace(result.Result) != "" {
			resolved = append(resolved, result)
			continue
		}
		rawURL := strings.TrimSpace(result.URL)
		if rawURL == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(rawURL), "data:") {
			result.Result = rawURL
			resolved = append(resolved, result)
			continue
		}
		if account == nil {
			continue
		}
		encoded, err := s.fetchOpenAIImageURLBase64(ctx, account, rawURL)
		if err != nil {
			logger.LegacyPrintf("service.openai_gateway", "[ImageLog] image URL download skipped account_id=%d err=%v", account.ID, err)
			continue
		}
		result.Result = encoded
		resolved = append(resolved, result)
	}
	return resolved
}
