package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	defaultPort       = 9000
	defaultConfigPath = "/etc/mdv/config.json"
	defaultSocketPath = "/run/mdv.sock"
	defaultUploadDir  = "/var/lib/mdv/upload"
	maxUploadSize     = 20 << 20 // 20MB per file
)

// configPath / socketPath honor env overrides so the server and CLI can be
// exercised without root (tests, local runs).
func configPath() string {
	if v := os.Getenv("MDV_CONFIG"); v != "" {
		return v
	}
	return defaultConfigPath
}

func socketPath() string {
	if v := os.Getenv("MDV_SOCKET"); v != "" {
		return v
	}
	return defaultSocketPath
}

type Config struct {
	Port      int      `json:"port"`
	External  bool     `json:"external"`
	Secret    string   `json:"secret"`
	Roots     []string `json:"roots"`
	UploadDir string   `json:"upload_dir"`
}

func loadConfig() (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(configPath())
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read config: %w", err)
		}
	} else if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", configPath(), err)
	}

	dirty := false
	if cfg.Port == 0 {
		cfg.Port = defaultPort
		dirty = true
	}
	if cfg.UploadDir == "" {
		cfg.UploadDir = defaultUploadDir
		dirty = true
	}
	if cfg.Secret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("generate secret: %w", err)
		}
		cfg.Secret = hex.EncodeToString(b)
		dirty = true
	}
	if dirty {
		if err := saveConfig(cfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func saveConfig(cfg *Config) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return os.Rename(tmp, path)
}
