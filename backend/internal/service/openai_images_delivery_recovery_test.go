package service

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type imageDeadlineBody struct{ ctx context.Context }

func (b imageDeadlineBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b imageDeadlineBody) Close() error             { return nil }

type imageTimeoutOnceUpstream struct {
	httpUpstreamRecorder
	calls int
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

func TestImageDeliveryReceiptResumesExistingImageAfterInitialFailure(t *testing.T) {
	root := t.TempDir()
	options := &openAIImagesResultDeliveryOptions{Mode: openAIImagesResultDeliveryFileURL, BaseURL: "https://relay.example.com", DataDir: root, JobID: "imgjob_resume", AccountID: 7, PreserveSource: true,
		FetchURL: func(string) ([]byte, error) { return nil, context.DeadlineExceeded }}
	_, err := applyOpenAIImagesFileURLDelivery([]byte(`{"data":[{"url":"https://cdn.example.com/a.png?private=secret"}]}`), options)
	require.Error(t, err)
	p := filepath.Join(root, "image_jobs", "imgjob_resume", "delivery-source.json")
	info, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	upstream := &httpUpstreamRecorder{resp: b64BackfillImageResponse(200, "image/png", b64BackfillPNGBytes)}
	cfg := &config.Config{}
	cfg.Pricing.DataDir = root
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, accountRepo: &openAIRecordUsageAccountRepoStub{account: b64BackfillAccount(true)}}
	result, found, err := svc.ResumeImageJobDelivery(context.Background(), "imgjob_resume")
	require.True(t, found)
	require.NoError(t, err)
	require.Contains(t, gjson.GetBytes(result, "data.0.url").String(), "relay.example.com/image-files/")
	require.Len(t, upstream.requests, 1)
	require.Equal(t, http.MethodGet, upstream.requests[0].Method)
	source, _, err := readImageDeliverySource(root, "imgjob_resume")
	require.NoError(t, err)
	require.Equal(t, 1, source.RecoveryAttempts)
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
	_, found, err := svc.ResumeImageJobDelivery(context.Background(), "imgjob_expired")
	require.True(t, found)
	require.Error(t, err)
	present, retry := svc.ImageJobDeliveryRecovery("imgjob_expired")
	require.True(t, present)
	require.False(t, retry)
	p, _ := imageDeliverySourcePath(root, "imgjob_corrupt")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0700))
	require.NoError(t, os.WriteFile(p, []byte("broken"), 0600))
	_, found, err = svc.ResumeImageJobDelivery(context.Background(), "imgjob_corrupt")
	require.True(t, found)
	require.Error(t, err)
	_, found, err = svc.ResumeImageJobDelivery(context.Background(), "imgjob_absent")
	require.False(t, found)
	require.NoError(t, err)
}
