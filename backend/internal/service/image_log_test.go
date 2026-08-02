package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func newTestImageLogService(t *testing.T, repo *fakeImageLogRepository) (*ImageLogService, string) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Pricing.DataDir = tmpDir
	return NewImageLogService(repo, cfg), tmpDir
}

func testPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 40, B: 80, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestRecordOpenAIImagesAcceptsDataURLAndCreatesThumbnail(t *testing.T) {
	repo := &fakeImageLogRepository{}
	svc, dataDir := newTestImageLogService(t, repo)
	pngBase64 := base64.StdEncoding.EncodeToString(testPNGBytes(t))

	err := svc.RecordOpenAIImages(context.Background(), &RecordImageLogInput{
		User:      &User{ID: 1, Email: "user@example.com"},
		APIKey:    &APIKey{ID: 2, Name: "image key"},
		RequestID: "req-test",
		Model:     "gpt-image-2",
		Prompt:    "one pixel",
		CreatedAt: time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC),
		Results: []openAIResponsesImageResult{
			{
				Result:       "data:image/png;base64," + pngBase64,
				OutputFormat: "png",
			},
		},
	})
	if err != nil {
		t.Fatalf("record image log: %v", err)
	}
	if len(repo.created) != 1 {
		t.Fatalf("expected one image log, got %d", len(repo.created))
	}
	if got := repo.created[0].ImageCount; got != 1 {
		t.Fatalf("image count mismatch: got %d", got)
	}
	img := repo.created[0].Images[0]
	if img.MIMEType != "image/png" {
		t.Fatalf("mime type mismatch: got %q", img.MIMEType)
	}
	if img.ThumbnailPath == "" {
		t.Fatalf("expected thumbnail path")
	}
	if img.Width != 2 || img.Height != 2 {
		t.Fatalf("dimensions mismatch: got %dx%d", img.Width, img.Height)
	}
	if _, err := os.Stat(filepath.Join(dataDir, img.FilePath)); err != nil {
		t.Fatalf("original file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, img.ThumbnailPath)); err != nil {
		t.Fatalf("thumbnail file missing: %v", err)
	}
}

func TestRecordOpenAIImagesSkipsDecodedNonImagePayload(t *testing.T) {
	repo := &fakeImageLogRepository{}
	svc, _ := newTestImageLogService(t, repo)

	err := svc.RecordOpenAIImages(context.Background(), &RecordImageLogInput{
		User:      &User{ID: 1},
		APIKey:    &APIKey{ID: 2},
		CreatedAt: time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC),
		Results: []openAIResponsesImageResult{
			{
				Result:       base64.StdEncoding.EncodeToString([]byte("not an image")),
				OutputFormat: "png",
			},
		},
	})
	if err != nil {
		t.Fatalf("record image log: %v", err)
	}
	if len(repo.created) != 0 {
		t.Fatalf("expected invalid payload to be skipped, got %d logs", len(repo.created))
	}
}

func TestThumbnailBytesRegeneratesBrokenThumbnailFromOriginal(t *testing.T) {
	repo := &fakeImageLogRepository{byID: map[int64]*ImageLog{}}
	svc, dataDir := newTestImageLogService(t, repo)

	originalRaw := testPNGBytes(t)
	originalRel := filepath.ToSlash(filepath.Join("image_logs", "original.png"))
	thumbRel := filepath.ToSlash(filepath.Join("image_logs", "broken_thumb.jpg"))
	if err := os.MkdirAll(filepath.Join(dataDir, "image_logs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, originalRel), originalRaw, 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, thumbRel), []byte("broken"), 0o644); err != nil {
		t.Fatalf("write broken thumbnail: %v", err)
	}

	repo.byID[9] = &ImageLog{
		ID: 9,
		Images: []ImageLogImage{
			{
				Index:         0,
				MIMEType:      "image/png",
				FilePath:      originalRel,
				ThumbnailPath: thumbRel,
			},
		},
	}

	data, mimeType, err := svc.ThumbnailBytes(context.Background(), 9, 0)
	if err != nil {
		t.Fatalf("thumbnail bytes: %v", err)
	}
	if mimeType != "image/jpeg" {
		t.Fatalf("mime type mismatch: got %q", mimeType)
	}
	if len(data) == 0 {
		t.Fatalf("expected generated thumbnail data")
	}
}

func TestImageLogServiceRejectsPathsOutsideDataDir(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	outsidePath := filepath.Join(root, "outside.png")
	outsideData := []byte("must not be disclosed")
	if err := os.WriteFile(outsidePath, outsideData, 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	tests := []struct {
		name string
		path string
	}{
		{name: "absolute", path: outsidePath},
		{name: "parent traversal", path: "../outside.png"},
		{name: "sibling namespace", path: "image_jobs/outside.png"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeImageLogRepository{byID: map[int64]*ImageLog{
				int64(i + 1): {
					ID:     int64(i + 1),
					Images: []ImageLogImage{{Index: 0, FilePath: tt.path, MIMEType: "image/png"}},
				},
			}}
			cfg := &config.Config{}
			cfg.Pricing.DataDir = dataDir
			svc := NewImageLogService(repo, cfg)

			data, _, err := svc.ImageBytes(context.Background(), int64(i+1), 0)
			if err == nil {
				t.Fatalf("expected unsafe path to be rejected")
			}
			if len(data) != 0 {
				t.Fatalf("unsafe path returned %d bytes", len(data))
			}
		})
	}

	imageLogsDir := filepath.Join(dataDir, "image_logs")
	if err := os.MkdirAll(imageLogsDir, 0o755); err != nil {
		t.Fatalf("mkdir image logs: %v", err)
	}
	symlinkPath := filepath.Join(imageLogsDir, "escape.png")
	if err := os.Symlink(outsidePath, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	repo := &fakeImageLogRepository{byID: map[int64]*ImageLog{
		99: {ID: 99, Images: []ImageLogImage{{Index: 0, FilePath: "image_logs/escape.png", MIMEType: "image/png"}}},
	}}
	cfg := &config.Config{}
	cfg.Pricing.DataDir = dataDir
	svc := NewImageLogService(repo, cfg)
	data, _, err := svc.ImageBytes(context.Background(), 99, 0)
	if err == nil || len(data) != 0 {
		t.Fatalf("symlink escape must be rejected, bytes=%d err=%v", len(data), err)
	}
}
