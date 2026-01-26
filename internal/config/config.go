// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

// Package config handles server configuration.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds the server configuration.
type Config struct {
	Server ServerConfig `json:"server"`
	Store  StoreConfig  `json:"store"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	Mode       string    `json:"mode"`        // "debug" or "release"
	SocketPath string    `json:"socket_path"` // Unix socket path (empty to disable)
	AdminKey   string    `json:"admin_key"`   // Admin key for store management (min 20 chars)
	TLS        TLSConfig `json:"tls"`         // TLS configuration (optional)
}

// TLSConfig holds TLS/HTTPS settings.
type TLSConfig struct {
	CertFile string `json:"cert_file"` // Path to TLS certificate file
	KeyFile  string `json:"key_file"`  // Path to TLS private key file
}

// StoreConfig holds default store settings.
type StoreConfig struct {
	BasePath       string `json:"base_path"`        // Base directory for all stores
	DataBlockSize  uint32 `json:"data_block_size"`  // Default data block size
	IndexBlockSize uint32 `json:"index_block_size"` // Default index block size
	NumBlocks      uint32 `json:"num_blocks"`       // Default number of blocks
}

// DefaultConfig returns configuration with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:       "0.0.0.0",
			Port:       21080,
			Mode:       "release",
			SocketPath: "/var/run/tsstore/tsstore.sock",
		},
		Store: StoreConfig{
			BasePath:       "./data",
			DataBlockSize:  4096,
			IndexBlockSize: 4096,
			NumBlocks:      1024,
		},
	}
}

// Load loads configuration from a JSON file.
// If the file doesn't exist, returns default configuration.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Save saves configuration to a JSON file.
func (c *Config) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// LoadFromEnv overrides config values from environment variables.
func (c *Config) LoadFromEnv() {
	if host := os.Getenv("TSSTORE_HOST"); host != "" {
		c.Server.Host = host
	}
	if port := os.Getenv("TSSTORE_PORT"); port != "" {
		// Parse port - simple for now
		var p int
		if _, err := parseEnvInt(port, &p); err == nil && p > 0 {
			c.Server.Port = p
		}
	}
	if mode := os.Getenv("TSSTORE_MODE"); mode != "" {
		c.Server.Mode = mode
	}
	if basePath := os.Getenv("TSSTORE_DATA_PATH"); basePath != "" {
		c.Store.BasePath = basePath
	}
	if socketPath := os.Getenv("TSSTORE_SOCKET_PATH"); socketPath != "" {
		c.Server.SocketPath = socketPath
	}
	if adminKey := os.Getenv("TSSTORE_ADMIN_KEY"); adminKey != "" {
		c.Server.AdminKey = adminKey
	}
	if tlsCert := os.Getenv("TSSTORE_TLS_CERT"); tlsCert != "" {
		c.Server.TLS.CertFile = tlsCert
	}
	if tlsKey := os.Getenv("TSSTORE_TLS_KEY"); tlsKey != "" {
		c.Server.TLS.KeyFile = tlsKey
	}
}

// TLSEnabled returns true if TLS is configured with both cert and key files.
func (c *Config) TLSEnabled() bool {
	return c.Server.TLS.CertFile != "" && c.Server.TLS.KeyFile != ""
}

func parseEnvInt(s string, v *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + int(c-'0')
	}
	*v = n
	return n, nil
}
