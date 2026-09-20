package service

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
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
		[]openAIResponsesImageResult{result}, 1, nil, result, "url", service.openAIImagesResultDeliveryOptions(ctx, parsed, nil),
	)
	require.NoError(t, err)
	url := gjson.GetBytes(body, "data.0.url").String()
	require.True(t, strings.HasPrefix(url, "https://sub2api.example/image-files/image_jobs/imgjob_test/public/"), url)
	rel := strings.TrimPrefix(url, "https://sub2api.example/image-files/")
	saved, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(rel)))
	require.NoError(t, err)
	require.Equal(t, png, saved)
}

func TestOpenAIImagesFileURLDeliveryDownloadsUpstreamImage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	png := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, make([]byte, 32)...)
	requested := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
		require.Equal(t, "/temporary/image.png", r.URL.Path)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	t.Cleanup(upstream.Close)

	dataDir := t.TempDir()
	delivery := &openAIImagesResultDeliveryOptions{
		Mode:    openAIImagesResultDeliveryFileURL,
		BaseURL: "https://sub2api.example",
		DataDir: dataDir,
		JobID:   "imgjob_url",
		FetchURL: func(rawURL string) ([]byte, error) {
			resp, err := http.Get(rawURL) //nolint:gosec // Test server URL is intentionally dynamic.
			if err != nil {
				return nil, err
			}
			defer func() { _ = resp.Body.Close() }()
			return io.ReadAll(resp.Body)
		},
	}
	result := openAIResponsesImageResult{URL: upstream.URL + "/temporary/image.png", OutputFormat: "png"}
	body, err := buildOpenAIImagesAPIResponseWithDelivery(
		[]openAIResponsesImageResult{result}, 1, nil, result, "url", delivery,
	)
	require.NoError(t, err)
	require.True(t, requested)
	publicURL := gjson.GetBytes(body, "data.0.url").String()
	require.True(t, strings.HasPrefix(publicURL, "https://sub2api.example/image-files/image_jobs/imgjob_url/public/"), publicURL)
	require.NotContains(t, string(body), upstream.URL)
	rel := strings.TrimPrefix(publicURL, "https://sub2api.example/image-files/")
	saved, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(rel)))
	require.NoError(t, err)
	require.Equal(t, png, saved)
}

func TestOpenAIImagesFileURLDeliveryUsesGatewayDownloader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	png := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, make([]byte, 32)...)
	upstream := &httpUpstreamRecorder{resp: b64BackfillImageResponse(http.StatusOK, "image/png", png)}
	dataDir := t.TempDir()
	service := &OpenAIGatewayService{
		cfg:          &config.Config{Pricing: config.PricingConfig{DataDir: dataDir}},
		httpUpstream: upstream,
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "https://sub2api.example/v1/images/generations", nil)
	ctx.Request.Host = "sub2api.example"
	ctx.Request.Header.Set("X-Sub2API-Image-Job-ID", "imgjob_gateway")
	parsed := &OpenAIImagesRequest{ResponseFormat: "url", ResultDelivery: "file_url"}
	account := &Account{ID: 7, Concurrency: 2}
	result := openAIResponsesImageResult{URL: "https://oss5.example/temporary.png", OutputFormat: "png"}

	body, err := buildOpenAIImagesAPIResponseWithDelivery(
		[]openAIResponsesImageResult{result}, 1, nil, result, "url", service.openAIImagesResultDeliveryOptions(ctx, parsed, account),
	)
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, result.URL, upstream.requests[0].URL.String())
	require.True(t, HTTPUpstreamPublicHostsOnly(upstream.requests[0].Context()))
	publicURL := gjson.GetBytes(body, "data.0.url").String()
	require.True(t, strings.HasPrefix(publicURL, "https://sub2api.example/image-files/image_jobs/imgjob_gateway/public/"), publicURL)
	require.NotContains(t, string(body), result.URL)
}

func TestOpenAIImagesFileURLDeliveryRejectsInvalidUpstreamImage(t *testing.T) {
	tests := []struct {
		name  string
		fetch func(string) ([]byte, error)
	}{
		{
			name: "download failure",
			fetch: func(string) ([]byte, error) {
				return nil, errors.New("temporary image unavailable")
			},
		},
		{
			name: "non image response",
			fetch: func(string) ([]byte, error) {
				return []byte("<html>not an image</html>"), nil
			},
		},
		{
			name: "oversized image",
			fetch: func(string) ([]byte, error) {
				payload := make([]byte, openAIImageMaxDownloadBytes+1)
				copy(payload, []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a})
				return payload, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delivery := &openAIImagesResultDeliveryOptions{
				Mode:     openAIImagesResultDeliveryFileURL,
				BaseURL:  "https://sub2api.example",
				DataDir:  t.TempDir(),
				JobID:    "imgjob_invalid",
				FetchURL: tt.fetch,
			}
			result := openAIResponsesImageResult{URL: "https://oss5.example/temporary.png", OutputFormat: "png"}
			body, err := buildOpenAIImagesAPIResponseWithDelivery(
				[]openAIResponsesImageResult{result}, 1, nil, result, "url", delivery,
			)
			require.Error(t, err)
			require.Nil(t, body)
		})
	}
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
