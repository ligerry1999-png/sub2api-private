package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const imageDeliveryRecoveryWindow = 5 * time.Minute
const imageDeliveryRecoveryLimit = 2

// Signed upstream URLs stay in the private job directory. Neither the public
// status response nor application logs may expose this receipt.
type imageDeliverySource struct {
	JobID            string          `json:"job_id"`
	AccountID        int64           `json:"account_id"`
	BaseURL          string          `json:"base_url"`
	CreatedAt        time.Time       `json:"created_at"`
	RecoveryAttempts int             `json:"recovery_attempts"`
	Response         json.RawMessage `json:"response"`
}

func imageDeliverySourcePath(dataDir, jobID string) (string, error) {
	if jobID == "" || safePublicImagePathSegment(jobID) != jobID {
		return "", errors.New("invalid image delivery job id")
	}
	if dataDir == "" {
		dataDir = "./data"
	}
	return filepath.Join(dataDir, "image_jobs", jobID, "delivery-source.json"), nil
}

// The caller supplied header alone is not authority to overwrite a private
// job receipt. It must match the authenticated owner of an admitted async job.
func canPreserveImageDeliverySource(dataDir, jobID string, apiKeyID int64) bool {
	path, err := imageDeliverySourcePath(dataDir, jobID)
	if err != nil || apiKeyID <= 0 {
		return false
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "meta.json"))
	if err != nil {
		return false
	}
	var meta struct {
		JobID    string `json:"job_id"`
		APIKeyID int64  `json:"api_key_id"`
	}
	return json.Unmarshal(data, &meta) == nil && meta.JobID == jobID && meta.APIKeyID == apiKeyID
}

func saveImageDeliverySource(dataDir string, source imageDeliverySource) error {
	path, err := imageDeliverySourcePath(dataDir, source.JobID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	if len(data) > 2<<20 {
		return errors.New("image delivery receipt is too large")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".delivery-source-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func readImageDeliverySource(dataDir, jobID string) (*imageDeliverySource, bool, error) {
	path, err := imageDeliverySourcePath(dataDir, jobID)
	if err != nil {
		return nil, true, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var source imageDeliverySource
	if len(data) > 2<<20 || json.Unmarshal(data, &source) != nil || source.JobID != jobID || source.CreatedAt.IsZero() {
		return nil, true, errors.New("invalid image delivery receipt")
	}
	return &source, true, nil
}

func (s *OpenAIGatewayService) imageDeliveryDataDir() string {
	if s != nil && s.cfg != nil && s.cfg.Pricing.DataDir != "" {
		return s.cfg.Pricing.DataDir
	}
	return "./data"
}

// ImageJobDeliveryRecovery reports presence separately from retry eligibility:
// a damaged/expired receipt must never fall through into paid generation.
func (s *OpenAIGatewayService) ImageJobDeliveryRecovery(jobID string) (present, retry bool) {
	source, present, err := readImageDeliverySource(s.imageDeliveryDataDir(), jobID)
	return present, err == nil && present && source.RecoveryAttempts < imageDeliveryRecoveryLimit && time.Now().Before(source.CreatedAt.Add(imageDeliveryRecoveryWindow))
}

// ResumeImageJobDelivery performs GETs for the already-created image only.
// The caller owns the existing job lease, so restart/retry cannot regenerate.
func (s *OpenAIGatewayService) ResumeImageJobDelivery(ctx context.Context, jobID string) ([]byte, bool, error) {
	dataDir := s.imageDeliveryDataDir()
	source, present, err := readImageDeliverySource(dataDir, jobID)
	if err != nil || !present {
		return nil, present, err
	}
	deadline := source.CreatedAt.Add(imageDeliveryRecoveryWindow)
	if source.RecoveryAttempts >= imageDeliveryRecoveryLimit || !time.Now().Before(deadline) {
		return nil, true, errors.New("image delivery recovery budget exhausted")
	}
	source.RecoveryAttempts++
	if err = saveImageDeliverySource(dataDir, *source); err != nil {
		return nil, true, err
	}
	if s == nil || s.accountRepo == nil {
		return nil, true, errors.New("image delivery account lookup is unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, source.AccountID)
	if err != nil || account == nil {
		return nil, true, errors.New("image delivery account is unavailable")
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	delivery := &openAIImagesResultDeliveryOptions{Mode: openAIImagesResultDeliveryFileURL, BaseURL: source.BaseURL,
		DataDir: dataDir, JobID: jobID, AccountID: source.AccountID, Resuming: true,
		FetchURL: func(rawURL string) ([]byte, error) { return s.fetchOpenAIImageURLBytes(ctx, account, rawURL) }}
	body, err := applyOpenAIImagesFileURLDelivery(source.Response, delivery)
	if err != nil {
		return nil, true, fmt.Errorf("resume existing image delivery: %w", err)
	}
	return body, true, nil
}
