package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
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

func testTransparentPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 200, G: 40, B: 80, A: 0})
	img.Set(1, 0, color.RGBA{R: 200, G: 40, B: 80, A: 128})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode transparent png: %v", err)
	}
	return buf.Bytes()
}

func testJPEGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	for x := 0; x < 2; x++ {
		img.Set(x, 0, color.RGBA{R: 200, G: 40, B: 80, A: 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func testPalettedTransparentPNGBytes(t *testing.T) []byte {
	t.Helper()
	palette := color.Palette{
		color.NRGBA{R: 10, G: 20, B: 30, A: 255},
		color.NRGBA{R: 40, G: 50, B: 60, A: 64},
	}
	img := image.NewPaletted(image.Rect(0, 0, 2, 1), palette)
	img.SetColorIndex(0, 0, 0)
	img.SetColorIndex(1, 0, 1)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode paletted png: %v", err)
	}
	return buf.Bytes()
}

func TestInspectImageAlphaDistinguishesTransparentPNGAndJPEG(t *testing.T) {
	transparent, err := inspectImageAlpha(testTransparentPNGBytes(t))
	if err != nil {
		t.Fatalf("inspect transparent png: %v", err)
	}
	if transparent.ColorMode != "RGBA" || !transparent.HasAlpha || transparent.AlphaMin == nil || *transparent.AlphaMin != 0 || transparent.AlphaMax == nil || *transparent.AlphaMax != 128 || !transparent.HasAlphaZero || !transparent.HasPartialAlpha {
		t.Fatalf("unexpected transparent png stats: %+v", transparent)
	}

	opaque, err := inspectImageAlpha(testJPEGBytes(t))
	if err != nil {
		t.Fatalf("inspect jpeg: %v", err)
	}
	if opaque.ColorMode != "RGB" || opaque.HasAlpha || opaque.AlphaMin != nil || opaque.AlphaMax != nil || opaque.HasAlphaZero || opaque.HasPartialAlpha {
		t.Fatalf("unexpected jpeg stats: %+v", opaque)
	}
}

func TestInspectImageAlphaRecognizesPalettedPNGTransparency(t *testing.T) {
	stats, err := inspectImageAlpha(testPalettedTransparentPNGBytes(t))
	if err != nil {
		t.Fatalf("inspect paletted png: %v", err)
	}
	if stats.ColorMode != "RGBA" || !stats.HasAlpha || stats.AlphaMin == nil || *stats.AlphaMin != 64 || stats.AlphaMax == nil || *stats.AlphaMax != 255 || !stats.HasPartialAlpha {
		t.Fatalf("unexpected paletted png stats: %+v", stats)
	}
}

func TestRecordOpenAIImagesStoresOriginalAlphaStats(t *testing.T) {
	repo := &fakeImageLogRepository{}
	svc, _ := newTestImageLogService(t, repo)
	pngBase64 := base64.StdEncoding.EncodeToString(testTransparentPNGBytes(t))

	err := svc.RecordOpenAIImages(context.Background(), &RecordImageLogInput{
		User:   &User{ID: 1, Email: "user@example.com"},
		APIKey: &APIKey{ID: 2, Name: "image key"},
		Results: []openAIResponsesImageResult{{
			Result:       "data:image/png;base64," + pngBase64,
			OutputFormat: "png",
		}},
	})
	if err != nil {
		t.Fatalf("record image log: %v", err)
	}
	if len(repo.created) != 1 || len(repo.created[0].Images) != 1 {
		t.Fatalf("expected one recorded image, got %#v", repo.created)
	}
	img := repo.created[0].Images[0]
	if img.ColorMode != "RGBA" || img.HasAlpha == nil || !*img.HasAlpha || img.AlphaMin == nil || *img.AlphaMin != 0 || img.AlphaMax == nil || *img.AlphaMax != 128 || !img.HasAlphaZero || !img.HasPartial || img.SHA256 == "" {
		t.Fatalf("unexpected stored alpha stats: %+v", img)
	}
}

func TestRecordOpenAIImagesFlagsBackgroundPixelMismatch(t *testing.T) {
	repo := &fakeImageLogRepository{}
	svc, _ := newTestImageLogService(t, repo)
	pngBase64 := base64.StdEncoding.EncodeToString(testPNGBytes(t))

	err := svc.RecordOpenAIImages(context.Background(), &RecordImageLogInput{
		User:     &User{ID: 1},
		APIKey:   &APIKey{ID: 2},
		Metadata: map[string]any{"background": "transparent"},
		Results: []openAIResponsesImageResult{{
			Result:       "data:image/png;base64," + pngBase64,
			OutputFormat: "png",
		}},
	})
	if err != nil {
		t.Fatalf("record image log: %v", err)
	}
	checks, ok := repo.created[0].Metadata["image_verification"].([]map[string]any)
	if !ok || len(checks) != 1 || checks[0]["status"] != "mismatch" {
		t.Fatalf("expected transparent background mismatch, got %#v", repo.created[0].Metadata["image_verification"])
	}
}

func TestRecordOpenAIImagesLocalizesTransparencyAfterMatchedForwarding(t *testing.T) {
	repo := &fakeImageLogRepository{}
	svc, _ := newTestImageLogService(t, repo)
	pngBase64 := base64.StdEncoding.EncodeToString(testTransparentPNGBytes(t))
	metadata := map[string]any{
		"background": "opaque",
		"effective": map[string]any{
			"model": "gpt-image-2", "background": "opaque", "output_format": "png", "n_effective": 1,
		},
		"forwarded": map[string]any{
			"model": "gpt-image-2", "background": "opaque", "output_format": "png", "n_effective": 1,
		},
		"result": []map[string]any{{
			"index": 0, "background": "transparent", "output_format": "png",
		}},
	}

	err := svc.RecordOpenAIImages(context.Background(), &RecordImageLogInput{
		User:     &User{ID: 1},
		APIKey:   &APIKey{ID: 2},
		Metadata: metadata,
		Results: []openAIResponsesImageResult{{
			Result:       "data:image/png;base64," + pngBase64,
			Background:   "transparent",
			OutputFormat: "png",
		}},
	})
	if err != nil {
		t.Fatalf("record image log: %v", err)
	}
	parameters, ok := repo.created[0].Metadata["parameter_verification"].(map[string]any)
	if !ok {
		t.Fatalf("expected parameter verification metadata, got %#v", repo.created[0].Metadata["parameter_verification"])
	}
	if parameters["status"] != "matched" {
		t.Fatalf("expected forwarding parameters to match, got %#v", parameters)
	}
	checks, ok := repo.created[0].Metadata["image_verification"].([]map[string]any)
	if !ok {
		t.Fatalf("expected image verification metadata, got %#v", repo.created[0].Metadata["image_verification"])
	}
	if len(checks) != 1 || checks[0]["status"] != "mismatch" || checks[0]["likely_transparency_stage"] != "upstream_generation_or_response" {
		t.Fatalf("expected upstream transparency localization, got %#v", checks)
	}
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
