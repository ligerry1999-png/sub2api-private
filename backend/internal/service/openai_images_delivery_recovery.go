package service

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Signed upstream URLs stay in the private job directory. Neither the public
// status response nor application logs may expose this receipt.
type imageDeliverySource struct {
	JobID     string          `json:"job_id"`
	AccountID int64           `json:"account_id"`
	BaseURL   string          `json:"base_url"`
	CreatedAt time.Time       `json:"created_at"`
	Response  json.RawMessage `json:"response"`
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
	defer func() { _ = os.Remove(f.Name()) }()
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

// ImageJobHasDeliverySource includes damaged receipts. After a process restart,
// delivery cannot bypass the original usage/billing/log finalization pipeline.
// Keep the source for reconciliation instead of regenerating or underbilling.
func (s *OpenAIGatewayService) ImageJobHasDeliverySource(jobID string) bool {
	_, present, _ := readImageDeliverySource(s.imageDeliveryDataDir(), jobID)
	return present
}
