package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// AccountExtraImagesURLToB64JSON 是账户 extra 中的开关键。开启后，Images 端点的非流式响应里
// b64_json 缺失或为空但 url 非空的图片项，由网关下载 url 内容并以 base64 回填到 b64_json。
const AccountExtraImagesURLToB64JSON = "images_url_to_b64_json"

// openAIImageURLDownloadTimeout 是单张图片 url 下载的超时上限。
const openAIImageURLDownloadTimeout = 60 * time.Second

// These retries fetch the same already-generated image URL. They never repeat
// the paid image-generation request.
const openAIImageURLDownloadMaxAttempts = 3

// ImagesURLToB64JSONEnabled 返回账户是否开启了 url 转 b64_json 回填。
func ImagesURLToB64JSONEnabled(account *Account) bool {
	return account != nil && account.getExtraBool(AccountExtraImagesURLToB64JSON)
}

// backfillOpenAIImagesB64JSON 对 Images 端点的非流式响应做 url 转 b64_json 回填。
//
// 只处理 data[i].b64_json 缺失或为空且 url 非空的项；url 字段原样保留。
// 单项失败仅记日志并保留该项原样，响应整体照常返回。
// 客户端显式要求 response_format=url 时不做回填。
func (s *OpenAIGatewayService) backfillOpenAIImagesB64JSON(
	ctx context.Context,
	account *Account,
	parsed *OpenAIImagesRequest,
	body []byte,
) []byte {
	if !ImagesURLToB64JSONEnabled(account) {
		return body
	}
	if parsed != nil && parsed.ResponseFormat == "url" {
		return body
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	items := gjson.GetBytes(body, "data")
	if !items.IsArray() {
		return body
	}
	for index, item := range items.Array() {
		if !item.IsObject() {
			continue
		}
		if strings.TrimSpace(item.Get("b64_json").String()) != "" {
			continue
		}
		rawURL := strings.TrimSpace(item.Get("url").String())
		if rawURL == "" {
			continue
		}
		encoded, err := s.fetchOpenAIImageURLBase64(ctx, account, rawURL)
		if err != nil {
			logger.LegacyPrintf(
				"service.openai_gateway",
				"[OpenAI] Images b64_json backfill skipped account_id=%d index=%d err=%s",
				account.ID,
				index,
				sanitizeUpstreamErrorMessage(err.Error()),
			)
			continue
		}
		updated, err := sjson.SetBytes(body, fmt.Sprintf("data.%d.b64_json", index), encoded)
		if err != nil {
			logger.LegacyPrintf(
				"service.openai_gateway",
				"[OpenAI] Images b64_json backfill skipped account_id=%d index=%d err=%s",
				account.ID,
				index,
				sanitizeUpstreamErrorMessage(err.Error()),
			)
			continue
		}
		body = updated
	}
	return body
}

// fetchOpenAIImageURLBase64 取得图片 url 内容的标准 base64 编码。
// data: 形式的 url 直接取其 base64 载荷；其余 url 先沿用 base_url 的出站 URL 策略校验，
// 再无条件拒绝回环、私网、链路本地等目的地（含重定向的每一跳），经账户代理下载，
// 大小上限与 OAuth 路径的单图下载一致，且前 512 字节须嗅探为 png/jpeg/webp/gif。
func (s *OpenAIGatewayService) fetchOpenAIImageURLBase64(ctx context.Context, account *Account, rawURL string) (string, error) {
	data, err := s.fetchOpenAIImageURLBytes(ctx, account, rawURL)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func (s *OpenAIGatewayService) fetchOpenAIImageURLBytes(ctx context.Context, account *Account, rawURL string) ([]byte, error) {
	if strings.HasPrefix(strings.ToLower(rawURL), "data:") {
		if encoded := normalizeOpenAIImageBase64(rawURL); encoded != "" {
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("decode data url payload: %w", err)
			}
			return data, nil
		}
		return nil, errors.New("data url payload is not valid base64")
	}
	if s == nil || s.httpUpstream == nil {
		return nil, errors.New("http upstream is not configured")
	}
	if account == nil {
		return nil, errors.New("image download account is not configured")
	}
	downloadURL, err := s.validateOutboundURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid image url: %w", err)
	}
	if err := rejectPrivateImageHost(downloadURL); err != nil {
		return nil, err
	}
	parsedURL, err := url.Parse(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("parse image url: %w", err)
	}
	host, port := parsedURL.Hostname(), parsedURL.Port()
	if port == "" {
		if parsedURL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return s.fetchOpenAIImageURLBytesWithTimeout(ctx, account, downloadURL, host, port, openAIImageURLDownloadTimeout)
}

func (s *OpenAIGatewayService) fetchOpenAIImageURLBytesWithTimeout(ctx context.Context, account *Account, downloadURL, host, port string, attemptTimeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(WithHTTPUpstreamPublicHostsOnly(ctx), time.Duration(openAIImageURLDownloadMaxAttempts)*attemptTimeout+3*time.Second)
	defer cancel()
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	started := time.Now()
	var attemptStarted time.Time
	var headerMs, bytesRead, contentLength int64
	logFailure := func(attempt int, status int, reason string) {
		logger.FromContext(ctx).Warn("openai.images.image_url_download_failed",
			zap.Int64("account_id", account.ID),
			zap.String("image_url_host", host),
			zap.String("image_url_port", port),
			zap.Int("image_url_http_status", status),
			zap.Int("download_attempts", attempt),
			zap.Int64("download_elapsed_ms", time.Since(started).Milliseconds()),
			zap.Int64("attempt_elapsed_ms", time.Since(attemptStarted).Milliseconds()),
			zap.Int64("response_header_ms", headerMs),
			zap.Int64("bytes_read", bytesRead),
			zap.Int64("content_length", contentLength),
			zap.String("failure_reason", reason),
		)
	}
	for attempt := 1; attempt <= openAIImageURLDownloadMaxAttempts; attempt++ {
		attemptStarted = time.Now()
		headerMs, bytesRead, contentLength = -1, 0, -1
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, attemptTimeout)
		defer cancelAttempt()
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, downloadURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build image download request: %w", err)
		}
		req.Header.Set("Accept", "image/*,*/*;q=0.8")
		resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
		headerMs = time.Since(attemptStarted).Milliseconds()
		if err != nil {
			cancelAttempt()
			logFailure(attempt, 0, "transport_error")
			if attempt < openAIImageURLDownloadMaxAttempts && ctx.Err() == nil && retryableOpenAIImageURLDownloadError(err) {
				if waitErr := waitOpenAIImageURLDownloadRetry(ctx, attempt); waitErr == nil {
					continue
				}
			}
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				err = urlErr.Err // Do not place a signed URL in application logs.
			}
			return nil, fmt.Errorf("download image: %w", err)
		}
		contentLength = resp.ContentLength
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			status := resp.StatusCode
			_ = resp.Body.Close()
			cancelAttempt()
			logFailure(attempt, status, "http_status")
			if attempt < openAIImageURLDownloadMaxAttempts && ctx.Err() == nil && retryableOpenAIImageURLDownloadStatus(status) {
				if waitErr := waitOpenAIImageURLDownloadRetry(ctx, attempt); waitErr == nil {
					continue
				}
			}
			return nil, fmt.Errorf("download image: unexpected status %d", status)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, openAIImageMaxDownloadBytes+1))
		bytesRead = int64(len(data))
		_ = resp.Body.Close()
		cancelAttempt()
		if readErr != nil {
			logFailure(attempt, resp.StatusCode, "read_error")
			if attempt < openAIImageURLDownloadMaxAttempts && ctx.Err() == nil && retryableOpenAIImageURLDownloadError(readErr) {
				if waitErr := waitOpenAIImageURLDownloadRetry(ctx, attempt); waitErr == nil {
					continue
				}
			}
			return nil, fmt.Errorf("read image body: %w", readErr)
		}
		if int64(len(data)) > openAIImageMaxDownloadBytes {
			return nil, fmt.Errorf("downloaded image exceeds %d bytes", openAIImageMaxDownloadBytes)
		}
		if len(data) == 0 {
			return nil, errors.New("download image: empty body")
		}
		if !isBackfillImageContent(data) {
			return nil, errors.New("download image: content is not an allowed image format")
		}
		logger.FromContext(ctx).Info("openai.images.image_url_download_completed",
			zap.Int64("account_id", account.ID), zap.String("image_url_host", host),
			zap.Int("download_attempts", attempt), zap.Int64("download_elapsed_ms", time.Since(started).Milliseconds()),
			zap.Int64("response_header_ms", headerMs), zap.Int64("bytes_read", bytesRead), zap.Int64("content_length", contentLength))
		return data, nil
	}
	return nil, errors.New("image download attempts exhausted")
}

func retryableOpenAIImageURLDownloadStatus(status int) bool {
	return status == http.StatusNotFound || status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func retryableOpenAIImageURLDownloadError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func waitOpenAIImageURLDownloadRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt) * 250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// rejectPrivateImageHost 拒绝主机为 localhost 或回环、私网、链路本地、未指定地址字面量的下载 URL。
// 不受 security.url_allowlist 配置影响；域名的解析结果与重定向的每一跳由上游客户端按
// WithHTTPUpstreamPublicHostsOnly 标记校验。
func rejectPrivateImageHost(downloadURL string) error {
	parsed, err := url.Parse(downloadURL)
	if err != nil {
		return fmt.Errorf("invalid image url: %w", err)
	}
	if host := parsed.Hostname(); urlvalidator.IsBlockedHost(host) {
		return fmt.Errorf("image url host is not allowed: %s", host)
	}
	return nil
}

// openAIImageBackfillContentTypes 是允许回填的图片格式，以字节嗅探结果为准，响应头不作为依据。
var openAIImageBackfillContentTypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/webp": {},
	"image/gif":  {},
}

// isBackfillImageContent 报告 data 的前 512 字节是否嗅探为允许回填的图片格式。
func isBackfillImageContent(data []byte) bool {
	_, ok := openAIImageBackfillContentTypes[detectedImageContentType(data)]
	return ok
}
