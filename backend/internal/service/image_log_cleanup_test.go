package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

type fakeImageLogRepository struct {
	expired    []ImageLog
	deletedIDs []int64
}

func (r *fakeImageLogRepository) Create(ctx context.Context, item *ImageLog) error {
	return nil
}

func (r *fakeImageLogRepository) List(ctx context.Context, params pagination.PaginationParams, filters ImageLogListFilter) ([]ImageLog, *pagination.PaginationResult, error) {
	return nil, &pagination.PaginationResult{}, nil
}

func (r *fakeImageLogRepository) GetByID(ctx context.Context, id int64) (*ImageLog, error) {
	return nil, nil
}

func (r *fakeImageLogRepository) ListExpired(ctx context.Context, cutoff time.Time, limit int) ([]ImageLog, error) {
	items := r.expired
	r.expired = nil
	return items, nil
}

func (r *fakeImageLogRepository) DeleteByIDs(ctx context.Context, ids []int64) (int64, error) {
	r.deletedIDs = append(r.deletedIDs, ids...)
	return int64(len(ids)), nil
}

func TestImageLogCleanupRunOnceDeletesRowsAndFiles(t *testing.T) {
	tmpDir := t.TempDir()
	original := filepath.Join(tmpDir, "image_logs", "old.png")
	thumb := filepath.Join(tmpDir, "image_logs", "old_thumb.jpg")
	if err := os.MkdirAll(filepath.Dir(original), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(original, []byte("original"), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
	if err := os.WriteFile(thumb, []byte("thumb"), 0o644); err != nil {
		t.Fatalf("write thumb: %v", err)
	}

	repo := &fakeImageLogRepository{
		expired: []ImageLog{
			{
				ID: 7,
				Images: []ImageLogImage{
					{FilePath: "image_logs/old.png", ThumbnailPath: "image_logs/old_thumb.jpg"},
				},
			},
		},
	}
	cfg := &config.Config{}
	cfg.Pricing.DataDir = tmpDir
	cfg.ImageLogs.CleanupEnabled = true
	cfg.ImageLogs.RetentionDays = 1
	cfg.ImageLogs.CleanupIntervalSeconds = 3600
	cfg.ImageLogs.CleanupBatchSize = 10

	svc := NewImageLogCleanupService(repo, nil, cfg)
	svc.runOnce()

	if !reflect.DeepEqual(repo.deletedIDs, []int64{7}) {
		t.Fatalf("deleted ids mismatch: got %v", repo.deletedIDs)
	}
	if _, err := os.Stat(original); !os.IsNotExist(err) {
		t.Fatalf("original image should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(thumb); !os.IsNotExist(err) {
		t.Fatalf("thumbnail should be removed, stat err=%v", err)
	}
}
