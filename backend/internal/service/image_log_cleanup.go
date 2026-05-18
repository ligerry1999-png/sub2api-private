package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const imageLogCleanupWorkerName = "image_log_cleanup_worker"

type ImageLogCleanupService struct {
	repo        ImageLogRepository
	timingWheel *TimingWheelService
	cfg         *config.Config
	dataDir     string

	running   int32
	startOnce sync.Once
	stopOnce  sync.Once

	workerCtx    context.Context
	workerCancel context.CancelFunc
}

func NewImageLogCleanupService(repo ImageLogRepository, timingWheel *TimingWheelService, cfg *config.Config) *ImageLogCleanupService {
	workerCtx, workerCancel := context.WithCancel(context.Background())
	return &ImageLogCleanupService{
		repo:         repo,
		timingWheel:  timingWheel,
		cfg:          cfg,
		dataDir:      resolveImageLogDataDir(cfg),
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
	}
}

func (s *ImageLogCleanupService) Start() {
	if s == nil {
		return
	}
	if !s.enabled() {
		logger.LegacyPrintf("service.image_log_cleanup", "[ImageLogCleanup] not started (disabled)")
		return
	}
	if s.repo == nil || s.timingWheel == nil {
		logger.LegacyPrintf("service.image_log_cleanup", "[ImageLogCleanup] not started (missing deps)")
		return
	}
	interval := s.interval()
	s.startOnce.Do(func() {
		s.timingWheel.ScheduleRecurring(imageLogCleanupWorkerName, interval, s.runOnce)
		logger.LegacyPrintf(
			"service.image_log_cleanup",
			"[ImageLogCleanup] started (retention_days=%d interval=%s batch_size=%d)",
			s.retentionDays(),
			interval,
			s.batchSize(),
		)
	})
}

func (s *ImageLogCleanupService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.workerCancel != nil {
			s.workerCancel()
		}
		if s.timingWheel != nil {
			s.timingWheel.Cancel(imageLogCleanupWorkerName)
		}
		logger.LegacyPrintf("service.image_log_cleanup", "[ImageLogCleanup] stopped")
	})
}

func (s *ImageLogCleanupService) runOnce() {
	if s == nil || !s.enabled() {
		return
	}
	if !atomic.CompareAndSwapInt32(&s.running, 0, 1) {
		logger.LegacyPrintf("service.image_log_cleanup", "[ImageLogCleanup] run_once skipped: already_running=true")
		return
	}
	defer atomic.StoreInt32(&s.running, 0)

	parent := context.Background()
	if s.workerCtx != nil {
		parent = s.workerCtx
	}
	ctx, cancel := context.WithTimeout(parent, s.taskTimeout())
	defer cancel()

	cutoff := time.Now().AddDate(0, 0, -s.retentionDays())
	batchSize := s.batchSize()
	var deletedRows int64
	var deletedFiles int

	for {
		if ctx.Err() != nil {
			logger.LegacyPrintf("service.image_log_cleanup", "[ImageLogCleanup] interrupted: err=%v deleted_rows=%d deleted_files=%d", ctx.Err(), deletedRows, deletedFiles)
			return
		}
		items, err := s.repo.ListExpired(ctx, cutoff, batchSize)
		if err != nil {
			logger.LegacyPrintf("service.image_log_cleanup", "[ImageLogCleanup] list expired failed: %v", err)
			return
		}
		if len(items) == 0 {
			if deletedRows > 0 || deletedFiles > 0 {
				logger.LegacyPrintf("service.image_log_cleanup", "[ImageLogCleanup] finished: deleted_rows=%d deleted_files=%d cutoff=%s", deletedRows, deletedFiles, cutoff.Format(time.RFC3339))
			}
			return
		}

		ids := make([]int64, 0, len(items))
		files := make([]string, 0, len(items)*2)
		for _, item := range items {
			if item.ID > 0 {
				ids = append(ids, item.ID)
			}
			for _, img := range item.Images {
				if p := strings.TrimSpace(img.FilePath); p != "" {
					files = append(files, p)
				}
				if p := strings.TrimSpace(img.ThumbnailPath); p != "" {
					files = append(files, p)
				}
			}
		}
		deleted, err := s.repo.DeleteByIDs(ctx, ids)
		if err != nil {
			logger.LegacyPrintf("service.image_log_cleanup", "[ImageLogCleanup] delete rows failed: %v", err)
			return
		}
		deletedRows += deleted
		deletedFiles += s.removeFiles(files)
		if len(items) < batchSize {
			logger.LegacyPrintf("service.image_log_cleanup", "[ImageLogCleanup] finished: deleted_rows=%d deleted_files=%d cutoff=%s", deletedRows, deletedFiles, cutoff.Format(time.RFC3339))
			return
		}
	}
}

func (s *ImageLogCleanupService) removeFiles(paths []string) int {
	seen := make(map[string]struct{}, len(paths))
	var removed int
	for _, path := range paths {
		path = s.storagePath(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		err := os.Remove(path)
		if err == nil {
			removed++
			continue
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		logger.LegacyPrintf("service.image_log_cleanup", "[ImageLogCleanup] remove file failed path=%s err=%v", path, err)
	}
	return removed
}

func (s *ImageLogCleanupService) storagePath(rel string) string {
	clean := filepath.Clean(strings.TrimSpace(rel))
	if clean == "." || clean == string(filepath.Separator) {
		return ""
	}
	if filepath.IsAbs(clean) {
		return clean
	}
	return filepath.Join(s.dataDir, clean)
}

func (s *ImageLogCleanupService) enabled() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	return s.cfg.ImageLogs.CleanupEnabled && s.retentionDays() > 0
}

func (s *ImageLogCleanupService) retentionDays() int {
	if s == nil || s.cfg == nil || s.cfg.ImageLogs.RetentionDays <= 0 {
		return 30
	}
	return s.cfg.ImageLogs.RetentionDays
}

func (s *ImageLogCleanupService) interval() time.Duration {
	seconds := 3600
	if s != nil && s.cfg != nil && s.cfg.ImageLogs.CleanupIntervalSeconds > 0 {
		seconds = s.cfg.ImageLogs.CleanupIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

func (s *ImageLogCleanupService) batchSize() int {
	if s == nil || s.cfg == nil || s.cfg.ImageLogs.CleanupBatchSize <= 0 {
		return 500
	}
	return s.cfg.ImageLogs.CleanupBatchSize
}

func (s *ImageLogCleanupService) taskTimeout() time.Duration {
	timeout := 5 * time.Minute
	if s != nil && s.interval() < timeout {
		timeout = s.interval()
	}
	if timeout < time.Second {
		timeout = time.Second
	}
	return timeout
}
