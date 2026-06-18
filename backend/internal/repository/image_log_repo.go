package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type imageLogRepository struct {
	db *sql.DB
}

func NewImageLogRepository(db *sql.DB) service.ImageLogRepository {
	return &imageLogRepository{db: db}
}

func (r *imageLogRepository) Create(ctx context.Context, item *service.ImageLog) error {
	if item == nil {
		return nil
	}
	imagesRaw, err := json.Marshal(item.Images)
	if err != nil {
		return err
	}
	metadataRaw, err := json.Marshal(item.Metadata)
	if err != nil {
		return err
	}
	createdAt := item.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	return r.db.QueryRowContext(ctx, `
		INSERT INTO image_logs (
			user_id, api_key_id, account_id, group_id, request_id, source, endpoint, model,
			prompt, status, error_message, image_count, image_size, duration_ms, images, metadata, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::jsonb, $16::jsonb, $17)
		RETURNING id
	`,
		item.UserID,
		item.APIKeyID,
		nullableInt64(item.AccountID),
		nullableInt64(item.GroupID),
		item.RequestID,
		item.Source,
		item.Endpoint,
		item.Model,
		item.Prompt,
		item.Status,
		item.ErrorMessage,
		item.ImageCount,
		item.ImageSize,
		item.DurationMs,
		string(imagesRaw),
		string(metadataRaw),
		createdAt,
	).Scan(&item.ID)
}

func (r *imageLogRepository) List(ctx context.Context, params pagination.PaginationParams, filters service.ImageLogListFilter) ([]service.ImageLog, *pagination.PaginationResult, error) {
	if params.Page < 1 {
		params.Page = 1
	}
	if params.PageSize < 1 {
		params.PageSize = 20
	}
	conditions, args := buildImageLogWhere(filters)
	whereSQL := "WHERE " + strings.Join(conditions, " AND ")

	var total int64
	countSQL := "SELECT COUNT(*) FROM image_logs il " + whereSQL
	if err := r.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, nil, err
	}

	limit := params.Limit()
	offset := params.Offset()
	queryArgs := append(append([]any{}, args...), limit, offset)
	rows, err := r.db.QueryContext(ctx, `
		SELECT
			il.id, il.user_id, il.api_key_id, il.account_id, il.group_id, il.request_id,
			il.source, il.endpoint, il.model, il.prompt, il.status, il.error_message,
			il.image_count, il.image_size, il.duration_ms, il.images, il.metadata, il.created_at,
			u.email, u.username, ak.name, a.name, g.name
		FROM image_logs il
		LEFT JOIN users u ON u.id = il.user_id
		LEFT JOIN api_keys ak ON ak.id = il.api_key_id
		LEFT JOIN accounts a ON a.id = il.account_id
		LEFT JOIN groups g ON g.id = il.group_id
		`+whereSQL+`
		ORDER BY il.created_at DESC, il.id DESC
		LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2),
		queryArgs...,
	)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]service.ImageLog, 0, limit)
	for rows.Next() {
		item, err := scanImageLog(rows)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	pages := int((total + int64(params.PageSize) - 1) / int64(params.PageSize))
	if pages < 1 {
		pages = 1
	}
	return items, &pagination.PaginationResult{
		Total:    total,
		Page:     params.Page,
		PageSize: params.PageSize,
		Pages:    pages,
	}, nil
}

func (r *imageLogRepository) GetByID(ctx context.Context, id int64) (*service.ImageLog, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT
			il.id, il.user_id, il.api_key_id, il.account_id, il.group_id, il.request_id,
			il.source, il.endpoint, il.model, il.prompt, il.status, il.error_message,
			il.image_count, il.image_size, il.duration_ms, il.images, il.metadata, il.created_at,
			u.email, u.username, ak.name, a.name, g.name
		FROM image_logs il
		LEFT JOIN users u ON u.id = il.user_id
		LEFT JOIN api_keys ak ON ak.id = il.api_key_id
		LEFT JOIN accounts a ON a.id = il.account_id
		LEFT JOIN groups g ON g.id = il.group_id
		WHERE il.id = $1
		LIMIT 1
	`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	item, err := scanImageLog(rows)
	if err != nil {
		return nil, err
	}
	return &item, rows.Err()
}

func (r *imageLogRepository) ListExpired(ctx context.Context, cutoff time.Time, limit int) ([]service.ImageLog, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, images
		FROM image_logs
		WHERE created_at < $1
		ORDER BY created_at ASC, id ASC
		LIMIT $2
	`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]service.ImageLog, 0, limit)
	for rows.Next() {
		var item service.ImageLog
		var imagesRaw []byte
		if err := rows.Scan(&item.ID, &imagesRaw); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(imagesRaw, &item.Images)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *imageLogRepository) DeleteByIDs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	if len(args) == 0 {
		return 0, nil
	}
	result, err := r.db.ExecContext(ctx, `DELETE FROM image_logs WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func buildImageLogWhere(filters service.ImageLogListFilter) ([]string, []any) {
	conditions := []string{"1=1"}
	args := make([]any, 0)
	if filters.UserID > 0 {
		args = append(args, filters.UserID)
		conditions = append(conditions, fmt.Sprintf("il.user_id = $%d", len(args)))
	}
	if filters.APIKeyID > 0 {
		args = append(args, filters.APIKeyID)
		conditions = append(conditions, fmt.Sprintf("il.api_key_id = $%d", len(args)))
	}
	if filters.AccountID > 0 {
		args = append(args, filters.AccountID)
		conditions = append(conditions, fmt.Sprintf("il.account_id = $%d", len(args)))
	}
	if filters.GroupID > 0 {
		args = append(args, filters.GroupID)
		conditions = append(conditions, fmt.Sprintf("il.group_id = $%d", len(args)))
	}
	if model := strings.TrimSpace(filters.Model); model != "" {
		args = append(args, model)
		conditions = append(conditions, fmt.Sprintf("il.model = $%d", len(args)))
	}
	if source := strings.TrimSpace(filters.Source); source != "" {
		args = append(args, source)
		conditions = append(conditions, fmt.Sprintf("il.source = $%d", len(args)))
	}
	if status := strings.TrimSpace(filters.Status); status != "" {
		args = append(args, status)
		conditions = append(conditions, fmt.Sprintf("il.status = $%d", len(args)))
	}
	if filters.StartTime != nil {
		args = append(args, *filters.StartTime)
		conditions = append(conditions, fmt.Sprintf("il.created_at >= $%d", len(args)))
	}
	if filters.EndTime != nil {
		args = append(args, *filters.EndTime)
		conditions = append(conditions, fmt.Sprintf("il.created_at < $%d", len(args)))
	}
	if query := strings.TrimSpace(filters.Query); query != "" {
		args = append(args, "%"+query+"%")
		conditions = append(conditions, fmt.Sprintf("(il.prompt ILIKE $%d OR il.request_id ILIKE $%d)", len(args), len(args)))
	}
	return conditions, args
}

type imageLogScanner interface {
	Scan(dest ...any) error
}

func scanImageLog(row imageLogScanner) (service.ImageLog, error) {
	var (
		item         service.ImageLog
		accountID    sql.NullInt64
		groupID      sql.NullInt64
		errorMessage sql.NullString
		imageSize    sql.NullString
		durationMs   sql.NullInt64
		imagesRaw    []byte
		metadataRaw  []byte
		userEmail    sql.NullString
		username     sql.NullString
		apiKeyName   sql.NullString
		accountName  sql.NullString
		groupName    sql.NullString
	)
	if err := row.Scan(
		&item.ID,
		&item.UserID,
		&item.APIKeyID,
		&accountID,
		&groupID,
		&item.RequestID,
		&item.Source,
		&item.Endpoint,
		&item.Model,
		&item.Prompt,
		&item.Status,
		&errorMessage,
		&item.ImageCount,
		&imageSize,
		&durationMs,
		&imagesRaw,
		&metadataRaw,
		&item.CreatedAt,
		&userEmail,
		&username,
		&apiKeyName,
		&accountName,
		&groupName,
	); err != nil {
		return service.ImageLog{}, err
	}
	if accountID.Valid {
		id := accountID.Int64
		item.AccountID = &id
	}
	if groupID.Valid {
		id := groupID.Int64
		item.GroupID = &id
	}
	if errorMessage.Valid {
		value := errorMessage.String
		item.ErrorMessage = &value
	}
	if imageSize.Valid {
		value := imageSize.String
		item.ImageSize = &value
	}
	if durationMs.Valid {
		value := int(durationMs.Int64)
		item.DurationMs = &value
	}
	_ = json.Unmarshal(imagesRaw, &item.Images)
	_ = json.Unmarshal(metadataRaw, &item.Metadata)
	if item.Metadata == nil {
		item.Metadata = map[string]any{}
	}
	item.User = &service.User{ID: item.UserID, Email: userEmail.String, Username: username.String}
	item.APIKey = &service.APIKey{ID: item.APIKeyID, Name: apiKeyName.String, UserID: item.UserID}
	if item.AccountID != nil {
		item.Account = &service.Account{ID: *item.AccountID, Name: accountName.String}
	}
	if item.GroupID != nil {
		item.Group = &service.Group{ID: *item.GroupID, Name: groupName.String}
	}
	return item, nil
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
