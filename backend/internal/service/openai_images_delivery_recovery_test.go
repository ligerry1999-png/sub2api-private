package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type imageDeadlineBody struct{ ctx context.Context }

func (b imageDeadlineBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b imageDeadlineBody) Close() error             { return nil }

type imageTimeoutOnceUpstream struct {
	httpUpstreamRecorder
	calls int
}

type imageReadFailureOnceUpstream struct{ httpUpstreamRecorder }

func (u *imageReadFailureOnceUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	u.requests = append(u.requests, req)
	if len(u.requests) == 1 {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: errReadCloser{err: io.ErrUnexpectedEOF}}, nil
	}
	return b64BackfillImageResponse(200, "image/png", b64BackfillPNGBytes), nil
}

func TestImageDownloadRecoveryReturnsOriginalUsageAndImageLogResults(t *testing.T) {
	upstream := &imageReadFailureOnceUpstream{}
	svc := &OpenAIGatewayService{cfg: &config.Config{Pricing: config.PricingConfig{DataDir: t.TempDir()}}, httpUpstream: upstream}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "https://relay.example.com/v1/images/generations", nil)
	c.Request.Header.Set("X-Image-Result-Delivery", "file_url")
	response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":5,"output_tokens":7},"data":[{"url":"https://cdn.example.com/a.png"}]}`))}
	usage, count, _, images, err := svc.handleOpenAIImagesNonStreamingResponse(context.Background(), response, c, b64BackfillAccount(true), &OpenAIImagesRequest{})
	require.NoError(t, err)
	require.Equal(t, 5, usage.InputTokens)
	require.Equal(t, 7, usage.OutputTokens)
	require.Equal(t, 1, count)
	require.Len(t, images, 1)
	require.Len(t, upstream.requests, 2)
	require.Contains(t, recorder.Body.String(), "/image-files/")
	for _, request := range upstream.requests {
		require.Equal(t, http.MethodGet, request.Method)
	}
}

func (u *imageTimeoutOnceUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	u.calls++
	u.requests = append(u.requests, req)
	if u.calls == 1 {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: imageDeadlineBody{req.Context()}}, nil
	}
	return b64BackfillImageResponse(200, "image/png", b64BackfillPNGBytes), nil
}

func TestImageDownloadTimeoutStartsFreshAttemptWithoutGeneration(t *testing.T) {
	upstream := &imageTimeoutOnceUpstream{}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	data, err := svc.fetchOpenAIImageURLBytesWithTimeout(context.Background(), b64BackfillAccount(true), "https://cdn.example.com/a.png", "cdn.example.com", "443", 10*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, b64BackfillPNGBytes, data)
	require.Equal(t, 2, upstream.calls)
	for _, req := range upstream.requests {
		require.Equal(t, http.MethodGet, req.Method)
	}
}

func TestImageDeliveryReceiptPreservesExistingImageAfterInitialFailure(t *testing.T) {
	root := t.TempDir()
	options := &openAIImagesResultDeliveryOptions{Mode: openAIImagesResultDeliveryFileURL, BaseURL: "https://relay.example.com", DataDir: root, JobID: "imgjob_resume", AccountID: 7, PreserveSource: true,
		FetchURL: func(string) ([]byte, error) { return nil, context.DeadlineExceeded }}
	_, err := applyOpenAIImagesFileURLDelivery([]byte(`{"data":[{"url":"https://cdn.example.com/a.png?private=secret"}]}`), options)
	require.Error(t, err)
	p := filepath.Join(root, "image_jobs", "imgjob_resume", "delivery-source.json")
	info, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	cfg := &config.Config{}
	cfg.Pricing.DataDir = root
	svc := &OpenAIGatewayService{cfg: cfg}
	require.True(t, svc.ImageJobHasDeliverySource("imgjob_resume"))
	source, found, err := readImageDeliverySource(root, "imgjob_resume")
	require.NoError(t, err)
	require.True(t, found)
	require.Contains(t, string(source.Response), "https://cdn.example.com/a.png?private=secret")
	require.EqualValues(t, 7, source.AccountID)
}

func TestImageDeliveryReceiptRequiresAdmittedJobAndMatchingOwner(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "image_jobs", "imgjob_owned", "meta.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0700))
	require.NoError(t, os.WriteFile(p, []byte(`{"job_id":"imgjob_owned","api_key_id":42}`), 0600))
	require.True(t, canPreserveImageDeliverySource(root, "imgjob_owned", 42))
	require.False(t, canPreserveImageDeliverySource(root, "imgjob_owned", 43))
	require.False(t, canPreserveImageDeliverySource(root, "imgjob_missing", 42))
	require.False(t, canPreserveImageDeliverySource(root, "../escape", 42))
}

func TestExpiredOrCorruptImageReceiptNeverPermitsRegeneration(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Pricing.DataDir = root
	svc := &OpenAIGatewayService{cfg: cfg}
	require.NoError(t, saveImageDeliverySource(root, imageDeliverySource{JobID: "imgjob_expired", CreatedAt: time.Now().Add(-10 * time.Minute)}))
	require.True(t, svc.ImageJobHasDeliverySource("imgjob_expired"))
	p, _ := imageDeliverySourcePath(root, "imgjob_corrupt")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0700))
	require.NoError(t, os.WriteFile(p, []byte("broken"), 0600))
	require.True(t, svc.ImageJobHasDeliverySource("imgjob_corrupt"))
	require.False(t, svc.ImageJobHasDeliverySource("imgjob_absent"))
}
