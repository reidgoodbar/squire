package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"squire/internal/protocol"
)

const defaultAPIBaseURL = "https://api.squire.run"

func DefaultAPIBaseURL() string {
	if value := os.Getenv("SQUIRE_API_BASE_URL"); value != "" {
		return value
	}
	return defaultAPIBaseURL
}

func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".squire", "config.json"), nil
}

func Load() (protocol.CLIConfig, error) {
	path, err := ConfigPath()
	if err != nil {
		return protocol.CLIConfig{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg := protocol.CLIConfig{APIBaseURL: DefaultAPIBaseURL()}
			if token := os.Getenv("SQUIRE_TOKEN"); token != "" {
				cfg.SessionToken = token
			}
			return cfg, nil
		}
		return protocol.CLIConfig{}, err
	}
	var cfg protocol.CLIConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return protocol.CLIConfig{}, err
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = DefaultAPIBaseURL()
	}
	if token := os.Getenv("SQUIRE_TOKEN"); token != "" {
		cfg.SessionToken = token
	}
	return cfg, nil
}

func Save(cfg protocol.CLIConfig) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = DefaultAPIBaseURL()
	}
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func Clear() error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
