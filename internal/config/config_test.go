// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package config

import (
	"strings"
	"testing"
)

func TestLoadFromEnvValidValues(t *testing.T) {
	t.Setenv("TSSTORE_PORT", "8080")
	t.Setenv("TSSTORE_MODE", "debug")
	t.Setenv("TSSTORE_HOST", "127.0.0.1")

	cfg := DefaultConfig()
	if err := cfg.LoadFromEnv(); err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Server.Port)
	}
	if cfg.Server.Mode != "debug" {
		t.Errorf("Mode = %q, want debug", cfg.Server.Mode)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1", cfg.Server.Host)
	}
}

func TestLoadFromEnvInvalidPort(t *testing.T) {
	for _, bad := range []string{"80a0", "-1", "0", "65536", "http"} {
		t.Setenv("TSSTORE_PORT", bad)
		cfg := DefaultConfig()
		err := cfg.LoadFromEnv()
		if err == nil {
			t.Errorf("TSSTORE_PORT=%q: expected error, got nil (port=%d)", bad, cfg.Server.Port)
			continue
		}
		if !strings.Contains(err.Error(), "TSSTORE_PORT") {
			t.Errorf("TSSTORE_PORT=%q: error should name the variable: %v", bad, err)
		}
		// The old behavior fell back to the default silently — make sure the
		// config wasn't half-applied either.
		if cfg.Server.Port != DefaultConfig().Server.Port {
			t.Errorf("TSSTORE_PORT=%q: port mutated to %d on error", bad, cfg.Server.Port)
		}
	}
}

func TestLoadFromEnvInvalidMode(t *testing.T) {
	t.Setenv("TSSTORE_MODE", "prod")
	cfg := DefaultConfig()
	err := cfg.LoadFromEnv()
	if err == nil {
		t.Fatal("TSSTORE_MODE=prod: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "TSSTORE_MODE") {
		t.Errorf("error should name the variable: %v", err)
	}
}

func TestLoadFromEnvUnsetLeavesDefaults(t *testing.T) {
	// t.Setenv with empty values ensures the vars are cleared for this test
	// even if the outer environment sets them.
	for _, k := range []string{"TSSTORE_HOST", "TSSTORE_PORT", "TSSTORE_MODE",
		"TSSTORE_DATA_PATH", "TSSTORE_SOCKET_PATH", "TSSTORE_ADMIN_KEY",
		"TSSTORE_TLS_CERT", "TSSTORE_TLS_KEY"} {
		t.Setenv(k, "")
	}
	cfg := DefaultConfig()
	if err := cfg.LoadFromEnv(); err != nil {
		t.Fatalf("LoadFromEnv with no env set: %v", err)
	}
	want := DefaultConfig()
	if cfg.Server != want.Server || cfg.Store != want.Store {
		t.Errorf("config changed with no env set: %+v", cfg)
	}
}
