package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/google/uuid"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	imageLogSourceChatGPT2API  = "chatgpt2api_worker"
	imageLogSourceSub2API      = "sub2api_native"
	imageLogSourceOpenAIAPIKey = "openai_api_key"
	imageLogSourceGrokMedia    = "grok_media"
	imageLogSourceResponses    = "sub2api_responses"
	imageLogStatusSuccess      = "success"

	imageLogThumbMaxSide = 360
)

type ImageLogImage struct {
	Index         int    `json:"index"`
	MIMEType      string `json:"mime_type"`
	FilePath      string `json:"file_path"`
	ThumbnailPath string `json:"thumbnail_path,omitempty"`
	SizeBytes     int64  `json:"size_bytes"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	ColorMode     string `json:"color_mode,omitempty"`
	HasAlpha      *bool  `json:"has_alpha,omitempty"`
	AlphaMin      *int   `json:"alpha_min,omitempty"`
	AlphaMax      *int   `json:"alpha_max,omitempty"`
	HasAlphaZero  bool   `json:"has_alpha_zero,omitempty"`
	HasPartial    bool   `json:"has_partial_alpha,omitempty"`
}

type ImageLog struct {
	ID           int64
	UserID       int64
	APIKeyID     int64
	AccountID    *int64
	GroupID      *int64
	RequestID    string
	Source       string
	Endpoint     string
	Model        string
	Prompt       string
	Status       string
	ErrorMessage *string
	ImageCount   int
	ImageSize    *string
	DurationMs   *int
	Images       []ImageLogImage
	Metadata     map[string]any
	CreatedAt    time.Time

	User    *User
	APIKey  *APIKey
	Account *Account
	Group   *Group
}

type ImageLogListFilter struct {
	UserID    int64
	APIKeyID  int64
	AccountID int64
	GroupID   int64
	Model     string
	Source    string
	Status    string
	Query     string
	StartTime *time.Time
	EndTime   *time.Time
}

type ImageLogRepository interface {
	Create(ctx context.Context, item *ImageLog) error
	List(ctx context.Context, params pagination.PaginationParams, filters ImageLogListFilter) ([]ImageLog, *pagination.PaginationResult, error)
	GetByID(ctx context.Context, id int64) (*ImageLog, error)
	ListExpired(ctx context.Context, cutoff time.Time, limit int) ([]ImageLog, error)
	DeleteByIDs(ctx context.Context, ids []int64) (int64, error)
}

type RecordImageLogInput struct {
	User         *User
	APIKey       *APIKey
	Account      *Account
	RequestID    string
	Source       string
	Endpoint     string
	Model        string
	Prompt       string
	ImageSize    string
	DurationMs   int
	Results      []ImageLogResult
	CreatedAt    time.Time
	Metadata     map[string]any
	ErrorMessage *string
}

type ImageLogService struct {
	repo    ImageLogRepository
	dataDir string
}

func NewImageLogService(repo ImageLogRepository, cfg *config.Config) *ImageLogService {
	return &ImageLogService{repo: repo, dataDir: resolveImageLogDataDir(cfg)}
}

func (s *ImageLogService) List(ctx context.Context, params pagination.PaginationParams, filters ImageLogListFilter) ([]ImageLog, *pagination.PaginationResult, error) {
	if s == nil || s.repo == nil {
		return nil, &pagination.PaginationResult{Total: 0, Page: params.Page, PageSize: params.PageSize, Pages: 1}, nil
	}
	return s.repo.List(ctx, params, filters)
}

func (s *ImageLogService) GetByID(ctx context.Context, id int64) (*ImageLog, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("image log service is not available")
	}
	return s.repo.GetByID(ctx, id)
}

func (s *ImageLogService) RecordOpenAIImages(ctx context.Context, input *RecordImageLogInput) error {
	if s == nil || s.repo == nil || input == nil || input.User == nil || input.APIKey == nil {
		return nil
	}
	if len(input.Results) == 0 {
		return nil
	}

	createdAt := input.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	images, err := s.saveImageResults(createdAt, input.Results)
	if err != nil {
		logger.LegacyPrintf("service.image_log", "[ImageLog] save images failed: %v", err)
		return err
	}
	if len(images) == 0 {
		return nil
	}
	attachImageLogVerification(input.Metadata, images)

	var accountID *int64
	if input.Account != nil && input.Account.ID > 0 {
		id := input.Account.ID
		accountID = &id
	}
	var groupID *int64
	if input.APIKey.GroupID != nil && *input.APIKey.GroupID > 0 {
		id := *input.APIKey.GroupID
		groupID = &id
	}

	size := strings.TrimSpace(input.ImageSize)
	var sizePtr *string
	if size != "" {
		sizePtr = &size
	}
	duration := input.DurationMs
	var durationPtr *int
	if duration >= 0 {
		durationPtr = &duration
	}

	item := &ImageLog{
		UserID:       input.User.ID,
		APIKeyID:     input.APIKey.ID,
		AccountID:    accountID,
		GroupID:      groupID,
		RequestID:    strings.TrimSpace(input.RequestID),
		Source:       defaultImageLogSource(input.Source),
		Endpoint:     strings.TrimSpace(input.Endpoint),
		Model:        strings.TrimSpace(input.Model),
		Prompt:       strings.TrimSpace(input.Prompt),
		Status:       imageLogStatusSuccess,
		ErrorMessage: input.ErrorMessage,
		ImageCount:   len(images),
		ImageSize:    sizePtr,
		DurationMs:   durationPtr,
		Images:       images,
		Metadata:     input.Metadata,
		CreatedAt:    createdAt,
	}
	if item.Metadata == nil {
		item.Metadata = map[string]any{}
	}
	return s.repo.Create(ctx, item)
}

func (s *ImageLogService) ThumbnailDataURL(item ImageLog, img ImageLogImage) string {
	data, mimeType, err := s.thumbnailBytesForImage(img)
	if err != nil {
		return ""
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func (s *ImageLogService) ImageBytes(ctx context.Context, id int64, index int) ([]byte, string, error) {
	item, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, "", err
	}
	for _, img := range item.Images {
		if img.Index != index {
			continue
		}
		if strings.TrimSpace(img.FilePath) == "" {
			return nil, "", fmt.Errorf("image file not found")
		}
		return s.readImageLogFile(img.FilePath, img.MIMEType)
	}
	return nil, "", fmt.Errorf("image index not found")
}

func (s *ImageLogService) ThumbnailBytes(ctx context.Context, id int64, index int) ([]byte, string, error) {
	item, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, "", err
	}
	for _, img := range item.Images {
		if img.Index != index {
			continue
		}
		return s.thumbnailBytesForImage(img)
	}
	return nil, "", fmt.Errorf("image index not found")
}

func (s *ImageLogService) thumbnailBytesForImage(img ImageLogImage) ([]byte, string, error) {
	if path := strings.TrimSpace(img.ThumbnailPath); path != "" {
		data, mimeType, err := s.readImageLogFile(path, "image/jpeg")
		if err == nil && imageLogPayloadIsDecodable(data) {
			return data, imageLogPayloadMIMEType("", mimeType, data), nil
		}
	}

	if strings.TrimSpace(img.FilePath) == "" {
		return nil, "", fmt.Errorf("image thumbnail not found")
	}
	original, mimeType, err := s.readImageLogFile(img.FilePath, img.MIMEType)
	if err != nil {
		return nil, "", err
	}
	thumb, _, _, err := buildImageLogThumbnail(original)
	if err == nil {
		return thumb, "image/jpeg", nil
	}
	if imageLogPayloadIsDecodable(original) {
		return original, imageLogPayloadMIMEType("", mimeType, original), nil
	}
	return nil, "", fmt.Errorf("image thumbnail is not decodable")
}

func (s *ImageLogService) readImageLogFile(path string, mimeType string) ([]byte, string, error) {
	fullPath, err := s.storagePath(path)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, "", err
	}
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	return data, mimeType, nil
}

func (s *ImageLogService) saveImageResults(createdAt time.Time, results []openAIResponsesImageResult) ([]ImageLogImage, error) {
	dayPrefix := filepath.Join("image_logs", createdAt.Format("2006"), createdAt.Format("01"), createdAt.Format("02"))
	outDir, err := s.storagePath(dayPrefix)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	images := make([]ImageLogImage, 0, len(results))
	batchID := uuid.NewString()
	for i, result := range results {
		raw, mimeType, err := decodeImageLogResultPayload(result)
		if err != nil {
			logger.LegacyPrintf("service.image_log", "[ImageLog] skip invalid image payload index=%d err=%v", i, err)
			continue
		}
		thumbData, width, height, err := buildImageLogThumbnail(raw)
		if err != nil {
			logger.LegacyPrintf("service.image_log", "[ImageLog] skip undecodable image payload index=%d err=%v", i, err)
			continue
		}
		alphaStats, err := inspectImageAlpha(raw)
		if err != nil {
			logger.LegacyPrintf("service.image_log", "[ImageLog] skip image alpha inspection index=%d err=%v", i, err)
			continue
		}
		ext := imageLogExtension(mimeType)
		name := fmt.Sprintf("%s_%02d%s", batchID, i, ext)
		relPath := filepath.ToSlash(filepath.Join(dayPrefix, name))
		fullPath, err := s.storagePath(relPath)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(fullPath, raw, 0o644); err != nil {
			return nil, err
		}
		thumbRel := filepath.ToSlash(filepath.Join(dayPrefix, fmt.Sprintf("%s_%02d_thumb.jpg", batchID, i)))
		thumbPath, err := s.storagePath(thumbRel)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(thumbPath, thumbData, 0o644); err != nil {
			return nil, err
		}

		logImage := ImageLogImage{
			Index:         i,
			MIMEType:      mimeType,
			FilePath:      relPath,
			ThumbnailPath: thumbRel,
			SizeBytes:     int64(len(raw)),
			Width:         width,
			Height:        height,
			SHA256:        sha256Hex(raw),
			ColorMode:     alphaStats.ColorMode,
			HasAlpha:      imageTraceBoolPtr(alphaStats.HasAlpha),
			AlphaMin:      alphaStats.AlphaMin,
			AlphaMax:      alphaStats.AlphaMax,
			HasAlphaZero:  alphaStats.HasAlphaZero,
			HasPartial:    alphaStats.HasPartialAlpha,
		}
		images = append(images, logImage)
	}
	return images, nil
}

func buildImageLogThumbnail(raw []byte) ([]byte, int, int, error) {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, err
	}
	bounds := src.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, width, height, fmt.Errorf("image has invalid dimensions")
	}
	targetW, targetH := width, height
	if width > imageLogThumbMaxSide || height > imageLogThumbMaxSide {
		if width >= height {
			targetW = imageLogThumbMaxSide
			targetH = max(1, height*imageLogThumbMaxSide/width)
		} else {
			targetH = imageLogThumbMaxSide
			targetW = max(1, width*imageLogThumbMaxSide/height)
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 78}); err != nil {
		return nil, width, height, err
	}
	return buf.Bytes(), width, height, nil
}

func decodeImageLogResultPayload(result openAIResponsesImageResult) ([]byte, string, error) {
	payload := strings.TrimSpace(result.Result)
	if payload == "" {
		return nil, "", fmt.Errorf("empty image payload")
	}
	raw, declaredMIME, err := decodeImageLogBase64Payload(payload)
	if err != nil {
		return nil, "", err
	}
	mimeType := imageLogPayloadMIMEType(result.OutputFormat, declaredMIME, raw)
	if !imageLogIsImageMIME(mimeType) {
		return nil, "", fmt.Errorf("decoded payload is not an image: %s", mimeType)
	}
	if !imageLogPayloadIsDecodable(raw) {
		return nil, "", fmt.Errorf("decoded payload is not a supported image")
	}
	return raw, mimeType, nil
}

func decodeImageLogBase64Payload(payload string) ([]byte, string, error) {
	lower := strings.ToLower(strings.TrimSpace(payload))
	if strings.HasPrefix(lower, "data:") {
		header, body, ok := strings.Cut(payload, ",")
		if !ok {
			return nil, "", fmt.Errorf("invalid image data URL")
		}
		mimeType := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(header, ";", 2)[0], "data:"))
		if mimeType != "" && !imageLogIsImageMIME(mimeType) {
			return nil, "", fmt.Errorf("data URL is not an image: %s", mimeType)
		}
		raw, err := base64.StdEncoding.DecodeString(cleanImageLogBase64(body))
		if err != nil {
			return nil, "", err
		}
		return raw, mimeType, nil
	}
	raw, err := base64.StdEncoding.DecodeString(cleanImageLogBase64(payload))
	if err != nil {
		return nil, "", err
	}
	return raw, "", nil
}

func cleanImageLogBase64(value string) string {
	replacer := strings.NewReplacer("\n", "", "\r", "", "\t", "", " ", "")
	return replacer.Replace(strings.TrimSpace(value))
}

func imageLogPayloadMIMEType(outputFormat string, declaredMIME string, raw []byte) string {
	if detected := http.DetectContentType(raw); imageLogIsImageMIME(detected) {
		return strings.ToLower(strings.TrimSpace(detected))
	}
	if imageLogIsImageMIME(declaredMIME) {
		return strings.ToLower(strings.TrimSpace(declaredMIME))
	}
	if candidate := openAIImageOutputMIMEType(outputFormat); imageLogIsImageMIME(candidate) {
		return strings.ToLower(strings.TrimSpace(candidate))
	}
	return strings.ToLower(strings.TrimSpace(http.DetectContentType(raw)))
}

func imageLogIsImageMIME(mimeType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/")
}

func imageLogPayloadIsDecodable(raw []byte) bool {
	_, _, err := image.Decode(bytes.NewReader(raw))
	return err == nil
}

func (s *ImageLogService) storagePath(rel string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("image log service is not available")
	}
	return resolveImageLogStoragePath(s.dataDir, rel)
}

func resolveImageLogStoragePath(dataDir, rel string) (string, error) {
	raw := strings.TrimSpace(rel)
	if raw == "" || filepath.IsAbs(raw) {
		return "", fmt.Errorf("image log path must be relative to the image log directory")
	}
	clean := filepath.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("image log path escapes the image log directory")
	}
	imageLogsPrefix := "image_logs"
	if clean != imageLogsPrefix && !strings.HasPrefix(clean, imageLogsPrefix+string(filepath.Separator)) {
		return "", fmt.Errorf("image log path is outside the image_logs namespace")
	}

	dataBase, err := filepath.Abs(filepath.Clean(strings.TrimSpace(dataDir)))
	if err != nil {
		return "", fmt.Errorf("resolve image log data directory: %w", err)
	}
	base := filepath.Join(dataBase, imageLogsPrefix)
	candidate, err := filepath.Abs(filepath.Join(dataBase, clean))
	if err != nil {
		return "", fmt.Errorf("resolve image log path: %w", err)
	}
	if !imageLogPathWithinBase(base, candidate) {
		return "", fmt.Errorf("image log path escapes the image log directory")
	}
	if err := ensureImageLogSymlinkPathWithinBase(base, candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

func imageLogPathWithinBase(base, candidate string) bool {
	relative, err := filepath.Rel(base, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func ensureImageLogSymlinkPathWithinBase(base, candidate string) error {
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("resolve image log root symlinks: %w", err)
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		if !imageLogPathWithinBase(resolvedBase, resolvedCandidate) {
			return fmt.Errorf("image log symlink escapes the image log directory")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("resolve image log path symlinks: %w", err)
	}

	// New files do not exist yet. Resolve the nearest existing parent so a
	// symlinked directory cannot redirect a write outside image_logs.
	for parent := filepath.Dir(candidate); ; parent = filepath.Dir(parent) {
		resolvedParent, parentErr := filepath.EvalSymlinks(parent)
		if parentErr == nil {
			if !imageLogPathWithinBase(resolvedBase, resolvedParent) {
				return fmt.Errorf("image log parent symlink escapes the image log directory")
			}
			return nil
		}
		if !os.IsNotExist(parentErr) {
			return fmt.Errorf("resolve image log parent symlinks: %w", parentErr)
		}
		if parent == base || filepath.Dir(parent) == parent {
			return nil
		}
	}
}

func resolveImageLogDataDir(cfg *config.Config) string {
	dataDir := "./data"
	if cfg != nil && strings.TrimSpace(cfg.Pricing.DataDir) != "" {
		dataDir = strings.TrimSpace(cfg.Pricing.DataDir)
	}
	return dataDir
}

func defaultImageLogSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return imageLogSourceSub2API
	}
	return source
}

func imageLogExtension(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}
