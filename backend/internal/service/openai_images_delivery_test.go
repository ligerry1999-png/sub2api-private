package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
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

func TestOpenAIImagesFileURLDeliveryAcceptsPaddedDataURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name string
		png  []byte
	}{
		{name: "one padding character", png: []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}},
		{name: "two padding characters", png: []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			service := &OpenAIGatewayService{
				cfg:          &config.Config{Pricing: config.PricingConfig{DataDir: dataDir}},
				httpUpstream: &httpUpstreamRecorder{},
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "https://sub2api.example/v1/images/edits", nil)
			ctx.Request.Host = "sub2api.example"
			ctx.Request.Header.Set("X-Sub2API-Image-Job-ID", "imgjob_padded_data_url")
			ctx.Request.Header.Set("X-Image-Result-Delivery", "file_url")
			parsed := &OpenAIImagesRequest{ResponseFormat: "url"}
			account := &Account{ID: 7, Concurrency: 2}
			dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(tt.png)
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"created":123,"data":[{"url":"` + dataURL + `"}]}`,
				)),
			}

			_, imageCount, _, _, err := service.handleOpenAIImagesNonStreamingResponse(
				context.Background(), response, ctx, account, parsed,
			)
			require.NoError(t, err)
			require.Equal(t, 1, imageCount)
			publicURL := gjson.GetBytes(recorder.Body.Bytes(), "data.0.url").String()
			require.True(t, strings.HasPrefix(publicURL, "https://sub2api.example/image-files/image_jobs/imgjob_padded_data_url/public/"), publicURL)
			rel := strings.TrimPrefix(publicURL, "https://sub2api.example/image-files/")
			saved, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(rel)))
			require.NoError(t, err)
			require.Equal(t, tt.png, saved)
		})
	}
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

type imageDownloadSequenceUpstream struct {
	requests  []*http.Request
	responses []*http.Response
	errs      []error
}

func (u *imageDownloadSequenceUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.requests = append(u.requests, req)
	if len(u.errs) > 0 {
		err := u.errs[0]
		u.errs = u.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(u.responses) == 0 {
		return nil, errors.New("unexpected extra image download")
	}
	response := u.responses[0]
	u.responses = u.responses[1:]
	return response, nil
}

func (u *imageDownloadSequenceUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func TestImageURLDownloadRetriesSameURLAfterConnectionRefused(t *testing.T) {
	upstream := &imageDownloadSequenceUpstream{
		errs: []error{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}},
		responses: []*http.Response{
			b64BackfillImageResponse(http.StatusOK, "image/png", b64BackfillPNGBytes),
		},
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	const imageURL = "https://cdn.example.com/image.png?signature=private"
	got, err := svc.fetchOpenAIImageURLBytes(context.Background(), b64BackfillAccount(true), imageURL)
	require.NoError(t, err)
	require.Equal(t, b64BackfillPNGBytes, got)
	require.Len(t, upstream.requests, 2)
	for _, request := range upstream.requests {
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, imageURL, request.URL.String())
	}
}

func TestImageURLDownloadRetries404ButNot401(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		wantCalls int
		wantErr   bool
	}{
		{name: "temporary 404", status: http.StatusNotFound, wantCalls: 2},
		{name: "401 needs authorization", status: http.StatusUnauthorized, wantCalls: 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &imageDownloadSequenceUpstream{responses: []*http.Response{
				b64BackfillImageResponse(tt.status, "text/plain", []byte("not ready")),
				b64BackfillImageResponse(http.StatusOK, "image/png", b64BackfillPNGBytes),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			got, err := svc.fetchOpenAIImageURLBytes(context.Background(), b64BackfillAccount(true), "https://cdn.example.com/image.png")
			if tt.wantErr {
				require.Error(t, err)
				require.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, b64BackfillPNGBytes, got)
			}
			require.Len(t, upstream.requests, tt.wantCalls)
		})
	}
}

func TestOpenAIImagesFileURLDeliveryFailureMarksPostUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{resp: b64BackfillImageResponse(http.StatusUnauthorized, "text/plain", []byte("unauthorized"))}
	svc := &OpenAIGatewayService{cfg: &config.Config{Pricing: config.PricingConfig{DataDir: t.TempDir()}}, httpUpstream: upstream}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "https://sub2api.example/v1/images/generations", nil)
	ctx.Request.Header.Set("X-Image-Result-Delivery", "file_url")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"created":123,"data":[{"url":"https://cdn.example.com/image.png"}]}`)),
	}
	_, _, _, _, err := svc.handleOpenAIImagesNonStreamingResponse(context.Background(), response, ctx, b64BackfillAccount(true), &OpenAIImagesRequest{})
	require.Error(t, err)
	require.Equal(t, "true", recorder.Header().Get("X-Sub2API-Image-Delivery-Failed"))
	require.True(t, ctx.GetBool(OpsImageDeliveryFailedKey))
	status, ok := ctx.Get(OpsUpstreamStatusCodeKey)
	require.True(t, ok)
	require.Equal(t, http.StatusOK, status)
}

func TestOpenAIImagesSuccessfulUpstreamBodyReadFailureStopsRegeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: errReadCloser{err: io.ErrUnexpectedEOF}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	_, _, _, _, err := svc.handleOpenAIImagesNonStreamingResponse(context.Background(), resp, ctx, b64BackfillAccount(false), &OpenAIImagesRequest{})
	require.Error(t, err)
	require.True(t, ctx.GetBool(OpsImageDeliveryFailedKey))
	require.Equal(t, "true", ctx.Writer.Header().Get(OpenAIImagesDeliveryFailureHeader))

	readErr := newOpenAIUpstreamStreamReadError(io.ErrUnexpectedEOF)
	require.ErrorIs(t, svc.handleOpenAIImagesOAuthResponseError(context.Background(), ctx, b64BackfillAccount(false), "gpt-image-2", "", resp, 0, readErr), readErr)
}

func TestOpenAIImagesAPIKeyResponseUsesFileURLDelivery(t *testing.T) {
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
	ctx.Request.Header.Set("X-Sub2API-Image-Job-ID", "imgjob_api_key")
	parsed := &OpenAIImagesRequest{ResponseFormat: "url", ResultDelivery: "file_url"}
	account := &Account{ID: 7, Concurrency: 2}
	providerURL := "https://oss5.example/temporary.png"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"created":123,"data":[{"url":"` + providerURL + `","size":"1024x1365"}]}`,
		)),
	}

	_, imageCount, _, _, err := service.handleOpenAIImagesNonStreamingResponse(
		context.Background(), response, ctx, account, parsed,
	)
	require.NoError(t, err)
	require.Equal(t, 1, imageCount)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, providerURL, upstream.requests[0].URL.String())
	body := recorder.Body.Bytes()
	publicURL := gjson.GetBytes(body, "data.0.url").String()
	require.True(t, strings.HasPrefix(publicURL, "https://sub2api.example/image-files/image_jobs/imgjob_api_key/public/"), publicURL)
	require.NotContains(t, string(body), providerURL)
	require.False(t, gjson.GetBytes(body, "data.0.b64_json").Exists())
	rel := strings.TrimPrefix(publicURL, "https://sub2api.example/image-files/")
	saved, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(rel)))
	require.NoError(t, err)
	require.Equal(t, png, saved)
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
