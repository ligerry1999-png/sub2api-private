package admin

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type ImageLogHandler struct {
	imageLogService *service.ImageLogService
}

func NewImageLogHandler(imageLogService *service.ImageLogService) *ImageLogHandler {
	return &ImageLogHandler{imageLogService: imageLogService}
}

type imageLogPersonDTO struct {
	ID       int64  `json:"id"`
	Email    string `json:"email,omitempty"`
	Username string `json:"username,omitempty"`
	Name     string `json:"name,omitempty"`
}

type imageLogImageDTO struct {
	Index        int    `json:"index"`
	MIMEType     string `json:"mime_type"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	SizeBytes    int64  `json:"size_bytes"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
}

type imageLogDTO struct {
	ID           int64              `json:"id"`
	UserID       int64              `json:"user_id"`
	APIKeyID     int64              `json:"api_key_id"`
	AccountID    *int64             `json:"account_id,omitempty"`
	GroupID      *int64             `json:"group_id,omitempty"`
	RequestID    string             `json:"request_id"`
	Source       string             `json:"source"`
	Endpoint     string             `json:"endpoint"`
	Model        string             `json:"model"`
	Prompt       string             `json:"prompt"`
	Status       string             `json:"status"`
	ErrorMessage *string            `json:"error_message,omitempty"`
	ImageCount   int                `json:"image_count"`
	ImageSize    *string            `json:"image_size,omitempty"`
	DurationMs   *int               `json:"duration_ms,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
	User         *imageLogPersonDTO `json:"user,omitempty"`
	APIKey       *imageLogPersonDTO `json:"api_key,omitempty"`
	Account      *imageLogPersonDTO `json:"account,omitempty"`
	Group        *imageLogPersonDTO `json:"group,omitempty"`
	Images       []imageLogImageDTO `json:"images"`
	Metadata     map[string]any     `json:"metadata,omitempty"`
}

func (h *ImageLogHandler) List(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	filters, ok := parseImageLogFilters(c)
	if !ok {
		return
	}
	params := pagination.PaginationParams{
		Page:      page,
		PageSize:  pageSize,
		SortBy:    "created_at",
		SortOrder: "desc",
	}
	items, result, err := h.imageLogService.List(c.Request.Context(), params, filters)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]imageLogDTO, 0, len(items))
	for i := range items {
		out = append(out, h.toDTO(items[i]))
	}
	response.Paginated(c, out, result.Total, page, pageSize)
}

func (h *ImageLogHandler) GetImage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid image log ID")
		return
	}
	index, err := strconv.Atoi(c.Param("index"))
	if err != nil || index < 0 {
		response.BadRequest(c, "Invalid image index")
		return
	}
	data, mimeType, err := h.imageLogService.ImageBytes(c.Request.Context(), id, index)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.NotFound(c, "Image log not found")
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	c.Data(http.StatusOK, mimeType, data)
}

func (h *ImageLogHandler) GetThumbnail(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid image log ID")
		return
	}
	index, err := strconv.Atoi(c.Param("index"))
	if err != nil || index < 0 {
		response.BadRequest(c, "Invalid image index")
		return
	}
	data, mimeType, err := h.imageLogService.ThumbnailBytes(c.Request.Context(), id, index)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.NotFound(c, "Image log not found")
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	c.Data(http.StatusOK, mimeType, data)
}

func (h *ImageLogHandler) toDTO(item service.ImageLog) imageLogDTO {
	images := make([]imageLogImageDTO, 0, len(item.Images))
	for _, img := range item.Images {
		images = append(images, imageLogImageDTO{
			Index:        img.Index,
			MIMEType:     img.MIMEType,
			ThumbnailURL: fmt.Sprintf("/admin/image-logs/%d/thumbnails/%d", item.ID, img.Index),
			SizeBytes:    img.SizeBytes,
			Width:        img.Width,
			Height:       img.Height,
		})
	}
	out := imageLogDTO{
		ID:           item.ID,
		UserID:       item.UserID,
		APIKeyID:     item.APIKeyID,
		AccountID:    item.AccountID,
		GroupID:      item.GroupID,
		RequestID:    item.RequestID,
		Source:       item.Source,
		Endpoint:     item.Endpoint,
		Model:        item.Model,
		Prompt:       item.Prompt,
		Status:       item.Status,
		ErrorMessage: item.ErrorMessage,
		ImageCount:   item.ImageCount,
		ImageSize:    item.ImageSize,
		DurationMs:   item.DurationMs,
		CreatedAt:    item.CreatedAt,
		Images:       images,
		Metadata:     item.Metadata,
	}
	if item.User != nil {
		out.User = &imageLogPersonDTO{ID: item.User.ID, Email: item.User.Email, Username: item.User.Username}
	}
	if item.APIKey != nil {
		out.APIKey = &imageLogPersonDTO{ID: item.APIKey.ID, Name: item.APIKey.Name}
	}
	if item.Account != nil {
		out.Account = &imageLogPersonDTO{ID: item.Account.ID, Name: item.Account.Name}
	}
	if item.Group != nil {
		out.Group = &imageLogPersonDTO{ID: item.Group.ID, Name: item.Group.Name}
	}
	return out
}

func parseImageLogFilters(c *gin.Context) (service.ImageLogListFilter, bool) {
	var filters service.ImageLogListFilter
	var ok bool
	if filters.UserID, ok = parseOptionalInt64Query(c, "user_id"); !ok {
		return filters, false
	}
	if filters.APIKeyID, ok = parseOptionalInt64Query(c, "api_key_id"); !ok {
		return filters, false
	}
	if filters.AccountID, ok = parseOptionalInt64Query(c, "account_id"); !ok {
		return filters, false
	}
	if filters.GroupID, ok = parseOptionalInt64Query(c, "group_id"); !ok {
		return filters, false
	}
	filters.Model = strings.TrimSpace(c.Query("model"))
	filters.Source = strings.TrimSpace(c.Query("source"))
	filters.Status = strings.TrimSpace(c.Query("status"))
	filters.Query = strings.TrimSpace(c.Query("q"))

	userTZ := c.Query("timezone")
	if start := strings.TrimSpace(c.Query("start_date")); start != "" {
		t, err := timezone.ParseInUserLocation("2006-01-02", start, userTZ)
		if err != nil {
			response.BadRequest(c, "Invalid start_date format, use YYYY-MM-DD")
			return filters, false
		}
		filters.StartTime = &t
	}
	if end := strings.TrimSpace(c.Query("end_date")); end != "" {
		t, err := timezone.ParseInUserLocation("2006-01-02", end, userTZ)
		if err != nil {
			response.BadRequest(c, "Invalid end_date format, use YYYY-MM-DD")
			return filters, false
		}
		t = t.AddDate(0, 0, 1)
		filters.EndTime = &t
	}
	return filters, true
}

func parseOptionalInt64Query(c *gin.Context, key string) (int64, bool) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		response.BadRequest(c, "Invalid "+key)
		return 0, false
	}
	return value, true
}
