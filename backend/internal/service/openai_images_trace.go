package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// summarizeOpenAIImagesRequestBody records only the fields needed to compare
// an image request across the gateway. It deliberately excludes prompt,
// uploads, credentials, and the raw JSON body.
func summarizeOpenAIImagesRequestBody(body []byte, endpoint string, effectiveN int) map[string]any {
	summary := map[string]any{
		"endpoint":    strings.TrimSpace(endpoint),
		"body_sha256": sha256Hex(body),
		"n_effective": effectiveN,
		"n_in_body":   false,
	}
	if !gjson.ValidBytes(body) {
		return summary
	}
	root := gjson.ParseBytes(body)
	imageOptions := root
	summary["payload_shape"] = "images"
	for _, tool := range root.Get("tools").Array() {
		if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "image_generation") {
			imageOptions = tool
			summary["payload_shape"] = "responses_image_tool"
			if driverModel := strings.TrimSpace(root.Get("model").String()); driverModel != "" {
				summary["driver_model"] = driverModel
			}
			break
		}
	}
	for _, key := range []string{"model", "size", "quality", "background", "output_format"} {
		if value := strings.TrimSpace(imageOptions.Get(key).String()); value != "" {
			summary[key] = value
		}
	}
	if responseFormat := strings.TrimSpace(root.Get("response_format").String()); responseFormat != "" {
		summary["response_format"] = responseFormat
	}
	if stream := root.Get("stream"); stream.Exists() && (stream.Type == gjson.True || stream.Type == gjson.False) {
		summary["stream"] = stream.Bool()
	}
	if n := imageOptions.Get("n"); n.Exists() {
		summary["n_in_body"] = true
		if n.Type == gjson.Number {
			summary["n"] = int(n.Int())
		}
	}
	return summary
}

// summarizeOpenAIImagesHTTPRequest snapshots the body immediately before the
// HTTP client sends it. The live request body is restored after reading so the
// trace cannot change the request that reaches the upstream.
func summarizeOpenAIImagesHTTPRequest(req *http.Request, effectiveN int) (map[string]any, error) {
	if req == nil {
		return nil, fmt.Errorf("request is required")
	}
	var (
		body []byte
		err  error
	)
	if req.Body != nil {
		body, err = io.ReadAll(req.Body)
		if err == nil {
			_ = req.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		}
	} else if req.GetBody != nil {
		var reader io.ReadCloser
		reader, err = req.GetBody()
		if err == nil {
			body, err = io.ReadAll(reader)
			_ = reader.Close()
		}
	}
	if err != nil {
		return nil, err
	}
	return summarizeOpenAIImagesRequestBody(body, requestPath(req), effectiveN), nil
}

func summarizeOpenAIImagesReceivedRequest(parsed *OpenAIImagesRequest) map[string]any {
	if parsed == nil {
		return nil
	}
	if !parsed.Multipart && gjson.ValidBytes(parsed.Body) {
		summary := summarizeOpenAIImagesRequestBody(parsed.Body, parsed.Endpoint, parsed.N)
		summary["capture"] = "raw_json"
		return summary
	}

	// Multipart values are read from the multipart body by the existing parser.
	// We still retain the exact body hash, but never store file bytes or prompt.
	summary := summarizeOpenAIImagesEffectiveRequest(parsed, parsed.Model)
	summary["capture"] = "parsed_multipart"
	summary["body_sha256"] = sha256Hex(parsed.Body)
	return summary
}

func summarizeOpenAIImagesEffectiveRequest(parsed *OpenAIImagesRequest, model string) map[string]any {
	if parsed == nil {
		return nil
	}
	summary := map[string]any{
		"endpoint":    strings.TrimSpace(parsed.Endpoint),
		"model":       strings.TrimSpace(model),
		"n_effective": parsed.N,
		"stream":      parsed.Stream,
	}
	for _, field := range []struct {
		key   string
		value string
	}{
		{"size", parsed.Size},
		{"quality", parsed.Quality},
		{"background", parsed.Background},
		{"output_format", parsed.OutputFormat},
		{"response_format", parsed.ResponseFormat},
	} {
		if value := strings.TrimSpace(field.value); value != "" {
			summary[field.key] = value
		}
	}
	return summary
}

func requestPath(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	return req.URL.Path
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func safeImageJobID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	value := strings.TrimSpace(c.GetHeader("X-Sub2API-Image-Job-ID"))
	if value == "" {
		idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
		if strings.HasPrefix(idempotencyKey, "image-job:") {
			value = strings.TrimPrefix(idempotencyKey, "image-job:")
		}
	}
	if len(value) == 0 || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return ""
		}
	}
	return value
}

func safeImageJobSubmitEndpoint(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	value := strings.TrimSpace(c.GetHeader("X-Sub2API-Image-Job-Submit-Endpoint"))
	switch value {
	case "/v1/image-jobs/images/generations", "/image-jobs/images/generations",
		"/v1/image-jobs/images/edits", "/image-jobs/images/edits":
		return value
	default:
		return ""
	}
}

type imageAlphaStats struct {
	ColorMode       string
	HasAlpha        bool
	AlphaMin        *int
	AlphaMax        *int
	HasAlphaZero    bool
	HasPartialAlpha bool
}

func inspectImageAlpha(raw []byte) (imageAlphaStats, error) {
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return imageAlphaStats{}, err
	}
	stats := imageAlphaStats{ColorMode: imageColorMode(decoded)}
	stats.HasAlpha = imageHasAlphaChannel(decoded)
	if !stats.HasAlpha {
		return stats, nil
	}

	minAlpha, maxAlpha := 255, 0
	for y := decoded.Bounds().Min.Y; y < decoded.Bounds().Max.Y; y++ {
		for x := decoded.Bounds().Min.X; x < decoded.Bounds().Max.X; x++ {
			_, _, _, alpha16 := decoded.At(x, y).RGBA()
			alpha := int((alpha16*255 + 32767) / 65535)
			if alpha < minAlpha {
				minAlpha = alpha
			}
			if alpha > maxAlpha {
				maxAlpha = alpha
			}
			if alpha == 0 {
				stats.HasAlphaZero = true
			}
			if alpha < 255 {
				stats.HasPartialAlpha = true
			}
		}
	}
	stats.AlphaMin = imageTraceIntPtr(minAlpha)
	stats.AlphaMax = imageTraceIntPtr(maxAlpha)
	return stats, nil
}

func imageColorMode(img image.Image) string {
	switch img.(type) {
	case *image.RGBA, *image.NRGBA, *image.RGBA64, *image.NRGBA64:
		return "RGBA"
	case *image.Gray, *image.Gray16:
		return "GRAY"
	case *image.Alpha, *image.Alpha16:
		return "ALPHA"
	case *image.Paletted:
		// PNGs with a tRNS chunk decode as a paletted image whose palette
		// entries carry alpha. Treat that as RGBA for the verification record;
		// reporting RGB here would hide real transparency.
		if imageHasAlphaChannel(img) {
			return "RGBA"
		}
		return "RGB"
	default:
		return "RGB"
	}
}

func imageHasAlphaChannel(img image.Image) bool {
	switch value := img.(type) {
	case *image.RGBA, *image.NRGBA, *image.RGBA64, *image.NRGBA64, *image.Alpha, *image.Alpha16:
		return true
	case *image.Paletted:
		for _, entry := range value.Palette {
			_, _, _, alpha := entry.RGBA()
			if alpha != 65535 {
				return true
			}
		}
	}
	return false
}

func imageTraceIntPtr(value int) *int {
	return &value
}

func imageTraceBoolPtr(value bool) *bool {
	return &value
}

func summarizeOpenAIImagesResults(results []openAIResponsesImageResult) []map[string]any {
	summary := make([]map[string]any, 0, len(results))
	for index, result := range results {
		item := map[string]any{"index": index}
		for _, field := range []struct {
			key   string
			value string
		}{
			{"model", result.Model},
			{"background", result.Background},
			{"output_format", result.OutputFormat},
			{"size", result.Size},
			{"quality", result.Quality},
		} {
			if value := strings.TrimSpace(field.value); value != "" {
				item[field.key] = value
			}
		}
		summary = append(summary, item)
	}
	return summary
}

func attachImageLogVerification(metadata map[string]any, images []ImageLogImage) {
	if metadata == nil || len(images) == 0 {
		return
	}
	received, _ := metadata["received"].(map[string]any)
	effective, _ := metadata["effective"].(map[string]any)
	if len(effective) == 0 {
		effective = received
	}
	forwarded := imageLogForwardedMetadata(metadata)
	forwarding := verifyImageLogParameters(effective, forwarded)
	forwarding["received_to_effective"] = compareImageLogStages(received, effective)
	metadata["parameter_verification"] = forwarding

	expectedBackground := strings.ToLower(strings.TrimSpace(metadataString(effective["background"])))
	expectedOutputFormat := strings.ToLower(strings.TrimSpace(metadataString(effective["output_format"])))
	if expectedBackground == "" {
		expectedBackground = strings.ToLower(strings.TrimSpace(metadataString(metadata["background"])))
	}
	if expectedOutputFormat == "" {
		expectedOutputFormat = strings.ToLower(strings.TrimSpace(metadataString(metadata["output_format"])))
	}
	resultMetadata := imageLogResultMetadata(metadata)
	checks := make([]map[string]any, 0, len(images))
	for _, image := range images {
		check := map[string]any{
			"index":                image.Index,
			"color_mode":           image.ColorMode,
			"actual_mime_type":     image.MIMEType,
			"actual_output_format": imageFormatFromMIMEType(image.MIMEType),
			"requested_background": expectedBackground,
		}
		resultBackground := ""
		resultOutputFormat := ""
		if image.Index >= 0 && image.Index < len(resultMetadata) {
			resultBackground = strings.ToLower(strings.TrimSpace(metadataString(resultMetadata[image.Index]["background"])))
			resultOutputFormat = strings.ToLower(strings.TrimSpace(metadataString(resultMetadata[image.Index]["output_format"])))
		}
		if resultBackground != "" {
			check["result_background"] = resultBackground
		}
		if expectedOutputFormat != "" {
			check["requested_output_format"] = expectedOutputFormat
		}
		if resultOutputFormat != "" {
			check["result_output_format"] = resultOutputFormat
		}
		if image.HasAlpha != nil {
			check["has_alpha"] = *image.HasAlpha
		}
		if image.AlphaMin != nil {
			check["alpha_min"] = *image.AlphaMin
		}
		if image.AlphaMax != nil {
			check["alpha_max"] = *image.AlphaMax
		}
		check["has_alpha_zero"] = image.HasAlphaZero
		check["has_partial_alpha"] = image.HasPartial
		requestBackgroundStatus := verifyImageLogBackground(expectedBackground, image)
		resultBackgroundStatus := verifyImageLogBackground(resultBackground, image)
		formatExpected := resultOutputFormat
		if formatExpected == "" {
			formatExpected = expectedOutputFormat
		}
		formatStatus := verifyImageLogOutputFormat(formatExpected, image.MIMEType)
		check["request_background_status"] = requestBackgroundStatus
		check["result_background_status"] = resultBackgroundStatus
		check["output_format_status"] = formatStatus
		check["status"] = combineImageLogVerificationStatus(requestBackgroundStatus, resultBackgroundStatus, formatStatus)
		if stage := likelyImageTransparencyStage(effective, forwarded, resultBackground, requestBackgroundStatus, resultBackgroundStatus); stage != "" {
			check["likely_transparency_stage"] = stage
		}
		checks = append(checks, check)
	}
	metadata["image_verification"] = checks
}

func imageLogResultMetadata(metadata map[string]any) []map[string]any {
	if results, ok := metadata["result"].([]map[string]any); ok {
		return results
	}
	values, ok := metadata["result"].([]any)
	if !ok {
		return nil
	}
	results := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if item, ok := value.(map[string]any); ok {
			results = append(results, item)
		}
	}
	return results
}

func imageLogForwardedMetadata(metadata map[string]any) map[string]any {
	if forwarded, ok := metadata["forwarded"].(map[string]any); ok {
		return forwarded
	}
	if attempts, ok := metadata["forwarded_attempts"].([]map[string]any); ok && len(attempts) > 0 {
		return attempts[len(attempts)-1]
	}
	if attempts, ok := metadata["forwarded_attempts"].([]any); ok {
		for index := len(attempts) - 1; index >= 0; index-- {
			if attempt, ok := attempts[index].(map[string]any); ok {
				return attempt
			}
		}
	}
	return nil
}

func verifyImageLogParameters(received, forwarded map[string]any) map[string]any {
	if len(received) == 0 || len(forwarded) == 0 {
		return map[string]any{"status": "not_available"}
	}
	mismatch := make([]string, 0, 5)
	for _, field := range []string{"model", "size", "quality", "background", "output_format"} {
		want := strings.TrimSpace(metadataString(received[field]))
		if want == "" {
			continue
		}
		got := strings.TrimSpace(metadataString(forwarded[field]))
		if !strings.EqualFold(want, got) {
			mismatch = append(mismatch, field)
		}
	}
	if n, ok := metadataInt(received["n_effective"]); ok && n > 0 {
		if got, ok := metadataInt(forwarded["n_effective"]); ok && got != n {
			mismatch = append(mismatch, "n")
		}
	}
	if len(mismatch) > 0 {
		return map[string]any{"status": "mismatch", "fields": mismatch}
	}
	return map[string]any{"status": "matched"}
}

func compareImageLogStages(received, effective map[string]any) map[string]any {
	if len(received) == 0 || len(effective) == 0 {
		return map[string]any{"status": "not_available"}
	}
	changed := make([]string, 0, 6)
	defaulted := make([]string, 0, 6)
	for _, field := range []string{"model", "size", "quality", "background", "output_format", "response_format"} {
		before := strings.TrimSpace(metadataString(received[field]))
		after := strings.TrimSpace(metadataString(effective[field]))
		switch {
		case before == "" && after != "":
			defaulted = append(defaulted, field)
		case before != "" && !strings.EqualFold(before, after):
			changed = append(changed, field)
		}
	}
	if before, ok := metadataInt(received["n_effective"]); ok {
		if after, afterOK := metadataInt(effective["n_effective"]); afterOK && before != after {
			changed = append(changed, "n")
		}
	}
	result := map[string]any{"status": "unchanged"}
	if len(changed) > 0 || len(defaulted) > 0 {
		result["status"] = "changed"
	}
	if len(changed) > 0 {
		result["changed_fields"] = changed
	}
	if len(defaulted) > 0 {
		result["defaulted_fields"] = defaulted
	}
	return result
}

func verifyImageLogBackground(expected string, image ImageLogImage) string {
	if expected != "transparent" && expected != "opaque" {
		return "not_checked"
	}
	hasTransparency := image.HasAlpha != nil && *image.HasAlpha && image.AlphaMin != nil && *image.AlphaMin < 255
	if expected == "transparent" && !hasTransparency {
		return "mismatch"
	}
	if expected == "opaque" && hasTransparency {
		return "mismatch"
	}
	return "matched"
}

func verifyImageLogOutputFormat(expected string, mimeType string) string {
	expected = normalizeImageFormat(expected)
	if expected == "" {
		return "not_checked"
	}
	if expected == normalizeImageFormat(imageFormatFromMIMEType(mimeType)) {
		return "matched"
	}
	return "mismatch"
}

func imageFormatFromMIMEType(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0])) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return ""
	}
}

func normalizeImageFormat(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "jpg" {
		return "jpeg"
	}
	return value
}

func combineImageLogVerificationStatus(statuses ...string) string {
	hasMatch := false
	for _, status := range statuses {
		if status == "mismatch" {
			return "mismatch"
		}
		if status == "matched" {
			hasMatch = true
		}
	}
	if hasMatch {
		return "matched"
	}
	return "not_checked"
}

func likelyImageTransparencyStage(effective, forwarded map[string]any, resultBackground, requestStatus, resultStatus string) string {
	expected := strings.ToLower(strings.TrimSpace(metadataString(effective["background"])))
	forwardedBackground := strings.ToLower(strings.TrimSpace(metadataString(forwarded["background"])))
	if expected != "" && forwardedBackground != "" && !strings.EqualFold(expected, forwardedBackground) {
		return "gateway_forwarding"
	}
	if forwardedBackground != "" && resultBackground != "" && !strings.EqualFold(forwardedBackground, resultBackground) {
		return "upstream_generation_or_response"
	}
	if requestStatus == "mismatch" || resultStatus == "mismatch" {
		return "upstream_image_bytes_or_metadata"
	}
	return ""
}

func metadataString(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func metadataInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	default:
		return 0, false
	}
}
