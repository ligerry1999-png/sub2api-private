package service

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIImagesFileURLDeliveryWritesPublicImage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dataDir := t.TempDir()
	service := &OpenAIGatewayService{cfg: &config.Config{Pricing: config.PricingConfig{DataDir: dataDir}}}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "https://sub2api.example/v1/images/generations", nil)
	ctx.Request.Host = "sub2api.example"
	ctx.Request.Header.Set("X-Sub2API-Image-Job-ID", "imgjob_test")

	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	result := openAIResponsesImageResult{Result: base64.StdEncoding.EncodeToString(png), OutputFormat: "png"}
	parsed := &OpenAIImagesRequest{ResponseFormat: "url", ResultDelivery: "file_url"}
	body, err := buildOpenAIImagesAPIResponseWithDelivery(
		[]openAIResponsesImageResult{result}, 1, nil, result, "url", service.openAIImagesResultDeliveryOptions(ctx, parsed),
	)
	require.NoError(t, err)
	url := gjson.GetBytes(body, "data.0.url").String()
	require.True(t, strings.HasPrefix(url, "https://sub2api.example/image-files/image_jobs/imgjob_test/public/"), url)
	rel := strings.TrimPrefix(url, "https://sub2api.example/image-files/")
	saved, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(rel)))
	require.NoError(t, err)
	require.Equal(t, png, saved)
}

func TestRewriteOpenAIImagesModelRemovesPrivateDeliveryField(t *testing.T) {
	body, contentType, err := rewriteOpenAIImagesModel(
		[]byte(`{"model":"gpt-image-2","prompt":"cat","result_delivery":"file_url"}`),
		"application/json",
		"gpt-image-2-codex",
	)
	require.NoError(t, err)
	require.Equal(t, "application/json", contentType)
	require.False(t, gjson.GetBytes(body, "result_delivery").Exists())
	require.Equal(t, "gpt-image-2-codex", gjson.GetBytes(body, "model").String())

	var input bytes.Buffer
	writer := multipart.NewWriter(&input)
	require.NoError(t, writer.WriteField("model", "gpt-image-2"))
	require.NoError(t, writer.WriteField("result_delivery", "file_url"))
	require.NoError(t, writer.WriteField("prompt", "cat"))
	require.NoError(t, writer.Close())
	rewritten, rewrittenType, err := rewriteOpenAIImagesModel(input.Bytes(), writer.FormDataContentType(), "gpt-image-2-codex")
	require.NoError(t, err)
	parsed := &OpenAIImagesRequest{}
	require.NoError(t, parseOpenAIImagesMultipartRequest(rewritten, rewrittenType, parsed))
	require.Empty(t, parsed.ResultDelivery)
	require.Equal(t, "gpt-image-2-codex", parsed.Model)
}
