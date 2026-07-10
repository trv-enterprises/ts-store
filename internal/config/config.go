// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package config handles server configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// LoadFromEnv overrides config values from environment variables. A set but
// invalid value is an error, not a silent fallback to the default — the
// serve usage text advertises these variables, so a typo'd value looking
// like it worked is worse than refusing to start.
func (c *Config) LoadFromEnv() error {
	if host := os.Getenv("TSSTORE_HOST"); host != "" {
		c.Server.Host = host
	}
	if port := os.Getenv("TSSTORE_PORT"); port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("invalid TSSTORE_PORT %q: expected a port number (1-65535)", port)
		}
		c.Server.Port = p
	}
	if mode := os.Getenv("TSSTORE_MODE"); mode != "" {
		// gin.SetMode panics on anything else — catch it at config time.
		if mode != "debug" && mode != "release" && mode != "test" {
			return fmt.Errorf("invalid TSSTORE_MODE %q: expected \"debug\" or \"release\"", mode)
		}
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
	return nil
}

// TLSEnabled returns true if TLS is configured with both cert and key files.
func (c *Config) TLSEnabled() bool {
	return c.Server.TLS.CertFile != "" && c.Server.TLS.KeyFile != ""
}
