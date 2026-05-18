package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var errOpenAIImagesWorkerUnsupported = errors.New("openai images worker unsupported request")

type openAIImagesWorkerPayload struct {
	AccessToken      string   `json:"access_token"`
	ChatGPTAccountID string   `json:"chatgpt_account_id,omitempty"`
	Model            string   `json:"model"`
	Prompt           string   `json:"prompt"`
	N                int      `json:"n"`
	Size             string   `json:"size,omitempty"`
	ResponseFormat   string   `json:"response_format"`
	Images           []string `json:"images,omitempty"`
}

func (s *OpenAIGatewayService) openAIImagesWorkerConfig() config.GatewayImageWorkerConfig {
	if s == nil || s.cfg == nil {
		return config.GatewayImageWorkerConfig{}
	}
	return s.cfg.Gateway.ImageWorker
}

func (s *OpenAIGatewayService) openAIImagesWorkerEnabled(parsed *OpenAIImagesRequest) bool {
	cfg := s.openAIImagesWorkerConfig()
	return parsed != nil &&
		cfg.Enabled &&
		strings.TrimSpace(cfg.BaseURL) != "" &&
		strings.TrimSpace(cfg.Token) != "" &&
		!parsed.Stream
}

func (s *OpenAIGatewayService) openAIImagesWorkerFallbackEnabled() bool {
	cfg := s.openAIImagesWorkerConfig()
	return cfg.FallbackEnabled
}

func buildOpenAIImagesWorkerInputImages(parsed *OpenAIImagesRequest) ([]string, error) {
	if parsed == nil {
		return nil, fmt.Errorf("parsed images request is required")
	}
	images := make([]string, 0, len(parsed.InputImageURLs)+len(parsed.Uploads))
	for _, imageURL := range parsed.InputImageURLs {
		trimmed := strings.TrimSpace(imageURL)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(trimmed), "data:") {
			return nil, errOpenAIImagesWorkerUnsupported
		}
		images = append(images, trimmed)
	}
	for _, upload := range parsed.Uploads {
		dataURL, err := openAIImageUploadToDataURL(upload)
		if err != nil {
			return nil, err
		}
		images = append(images, dataURL)
	}
	if parsed.IsEdits() && len(images) == 0 {
		return nil, fmt.Errorf("image input is required")
	}
	if parsed.HasMask || strings.TrimSpace(parsed.MaskImageURL) != "" || parsed.MaskUpload != nil {
		return nil, errOpenAIImagesWorkerUnsupported
	}
	return images, nil
}

func buildOpenAIImagesWorkerPayload(account *Account, parsed *OpenAIImagesRequest, requestModel string, accessToken string) ([]byte, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("access token is required")
	}
	images, err := buildOpenAIImagesWorkerInputImages(parsed)
	if err != nil {
		return nil, err
	}
	n := parsed.N
	if n <= 0 {
		n = 1
	}
	payload := openAIImagesWorkerPayload{
		AccessToken:      strings.TrimSpace(accessToken),
		ChatGPTAccountID: account.GetChatGPTAccountID(),
		Model:            strings.TrimSpace(requestModel),
		Prompt:           strings.TrimSpace(parsed.Prompt),
		N:                n,
		Size:             strings.TrimSpace(parsed.Size),
		ResponseFormat:   "b64_json",
		Images:           images,
	}
	return json.Marshal(payload)
}

func openAIImagesWorkerEndpoint(parsed *OpenAIImagesRequest) string {
	if parsed != nil && parsed.IsEdits() {
		return "/images/edits"
	}
	return "/images/generations"
}

func openAIImagesWorkerTimeout(cfg config.GatewayImageWorkerConfig) time.Duration {
	if cfg.TimeoutSeconds <= 0 {
		return 900 * time.Second
	}
	return time.Duration(cfg.TimeoutSeconds) * time.Second
}

func collectOpenAIImagesFromWorkerBody(body []byte, requestModel string) ([]openAIResponsesImageResult, int64, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, fmt.Errorf("invalid worker response")
	}
	createdAt := gjson.GetBytes(body, "created").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	results := make([]openAIResponsesImageResult, 0)
	for _, item := range gjson.GetBytes(body, "data").Array() {
		b64 := strings.TrimSpace(item.Get("b64_json").String())
		if b64 == "" {
			url := strings.TrimSpace(item.Get("url").String())
			lowerURL := strings.ToLower(url)
			const marker = ";base64,"
			if strings.HasPrefix(lowerURL, "data:") {
				if idx := strings.Index(lowerURL, marker); idx >= 0 {
					b64 = strings.TrimSpace(url[idx+len(marker):])
				}
			}
		}
		if b64 == "" {
			continue
		}
		results = append(results, openAIResponsesImageResult{
			Result:        b64,
			RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
			OutputFormat:  "png",
			Model:         strings.TrimSpace(requestModel),
		})
	}
	if len(results) == 0 {
		message := strings.TrimSpace(gjson.GetBytes(body, "message").String())
		if message == "" {
			message = "worker returned no image"
		}
		return nil, createdAt, errors.New(message)
	}
	return results, createdAt, nil
}

func (s *OpenAIGatewayService) forwardOpenAIImagesViaWorker(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	parsed *OpenAIImagesRequest,
	requestModel string,
	accessToken string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	cfg := s.openAIImagesWorkerConfig()
	payload, err := buildOpenAIImagesWorkerPayload(account, parsed, requestModel, accessToken)
	if err != nil {
		return nil, err
	}

	workerCtx, cancel := context.WithTimeout(ctx, openAIImagesWorkerTimeout(cfg))
	defer cancel()

	targetURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/") + openAIImagesWorkerEndpoint(parsed)
	req, err := http.NewRequestWithContext(workerCtx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Internal-Token", strings.TrimSpace(cfg.Token))

	workerStart := time.Now()
	upstreamStart := time.Now()
	resp, err := http.DefaultClient.Do(req)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("image worker request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readUpstreamResponseBodyLimited(resp.Body, resolveUpstreamResponseReadLimit(s.cfg))
	if err != nil {
		return nil, fmt.Errorf("read image worker response failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(body))
		if upstreamMsg == "" {
			upstreamMsg = resp.Status
		}
		return nil, fmt.Errorf("image worker failed: status=%d message=%s", resp.StatusCode, upstreamMsg)
	}

	results, createdAt, err := collectOpenAIImagesFromWorkerBody(body, requestModel)
	if err != nil {
		return nil, err
	}
	firstMeta := results[0]
	firstMeta.Size = strings.TrimSpace(parsed.Size)
	responseBody, err := buildOpenAIImagesAPIResponse(results, createdAt, nil, firstMeta, parsed.ResponseFormat)
	if err != nil {
		return nil, err
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", responseBody)
	requestID := resp.Header.Get("x-request-id")
	s.recordOpenAIImagesLog(ctx, c, account, parsed, requestModel, imageLogSourceChatGPT2API, requestID, startTime, results, map[string]any{
		"route":              imageLogSourceChatGPT2API,
		"worker_duration_ms": time.Since(workerStart).Milliseconds(),
	})

	logger.LegacyPrintf(
		"service.openai_gateway",
		"[OpenAI] Images worker bridge success endpoint=%s request_model=%s account_id=%d image_count=%d",
		parsed.Endpoint,
		requestModel,
		account.ID,
		len(results),
	)
	return &OpenAIForwardResult{
		RequestID:       requestID,
		Model:           requestModel,
		UpstreamModel:   requestModel,
		Stream:          false,
		ResponseHeaders: resp.Header.Clone(),
		Duration:        time.Since(startTime),
		ImageCount:      len(results),
		ImageSize:       parsed.SizeTier,
	}, nil
}
