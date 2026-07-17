package green

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const trustRelativePath = "green/trust.json"

var ErrConfigUntrusted = errors.New("Squire Green config is not trusted")

func TrustConfig(repoRoot, storeRoot string) (TrustRecord, error) {
	canonical, err := canonicalRepoRoot(repoRoot)
	if err != nil {
		return TrustRecord{}, err
	}
	config, err := LoadConfig(canonical)
	if err != nil {
		return TrustRecord{}, err
	}
	record := TrustRecord{
		Version:      stateVersion,
		RepoRoot:     canonical,
		ConfigDigest: config.Digest,
		TrustedAt:    time.Now(),
	}
	path := trustPath(storeRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return TrustRecord{}, err
	}
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return TrustRecord{}, err
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf("trust.%d.%d.tmp", os.Getpid(), time.Now().UnixNano()))
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return TrustRecord{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return TrustRecord{}, err
	}
	return record, nil
}

func ConfigTrusted(repoRoot, storeRoot, configDigest string) (bool, error) {
	canonical, err := canonicalRepoRoot(repoRoot)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(trustPath(storeRoot))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var record TrustRecord
	if err := json.Unmarshal(b, &record); err != nil {
		return false, fmt.Errorf("decode Green trust record: %w", err)
	}
	return record.Version == stateVersion &&
		filepath.Clean(record.RepoRoot) == canonical &&
		record.ConfigDigest != "" &&
		record.ConfigDigest == configDigest, nil
}

func RevokeConfigTrust(storeRoot string) error {
	err := os.Remove(trustPath(storeRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func trustPath(storeRoot string) string {
	return filepath.Join(storeRoot, filepath.FromSlash(trustRelativePath))
}
