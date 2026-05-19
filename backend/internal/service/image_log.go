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
	imageLogSourceChatGPT2API = "chatgpt2api_worker"
	imageLogSourceSub2API     = "sub2api_native"
	imageLogStatusSuccess     = "success"

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
	Results      []openAIResponsesImageResult
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
	data, err := os.ReadFile(s.storagePath(path))
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
	outDir := s.storagePath(dayPrefix)
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
		ext := imageLogExtension(mimeType)
		name := fmt.Sprintf("%s_%02d%s", batchID, i, ext)
		relPath := filepath.ToSlash(filepath.Join(dayPrefix, name))
		if err := os.WriteFile(s.storagePath(relPath), raw, 0o644); err != nil {
			return nil, err
		}

		logImage := ImageLogImage{
			Index:     i,
			MIMEType:  mimeType,
			FilePath:  relPath,
			SizeBytes: int64(len(raw)),
		}
		thumbRel, width, height := s.writeThumbnail(dayPrefix, batchID, i, raw)
		logImage.ThumbnailPath = thumbRel
		logImage.Width = width
		logImage.Height = height
		images = append(images, logImage)
	}
	return images, nil
}

func (s *ImageLogService) writeThumbnail(dayPrefix, batchID string, index int, raw []byte) (string, int, int) {
	data, width, height, err := buildImageLogThumbnail(raw)
	if err != nil {
		return "", width, height
	}
	relPath := filepath.ToSlash(filepath.Join(dayPrefix, fmt.Sprintf("%s_%02d_thumb.jpg", batchID, index)))
	if err := os.WriteFile(s.storagePath(relPath), data, 0o644); err != nil {
		return "", width, height
	}
	return relPath, width, height
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
	_, _, err := image.DecodeConfig(bytes.NewReader(raw))
	return err == nil
}

func (s *ImageLogService) storagePath(rel string) string {
	clean := filepath.Clean(strings.TrimSpace(rel))
	if clean == "." || clean == string(filepath.Separator) {
		return filepath.Clean(s.dataDir)
	}
	if filepath.IsAbs(clean) {
		return clean
	}
	return filepath.Join(s.dataDir, clean)
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
