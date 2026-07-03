// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression tests for issue #12: store names are joined onto the data path
// and used for MkdirAll / os.RemoveAll, so traversal-capable names must be
// rejected at every entry point.

func TestValidateStoreName(t *testing.T) {
	valid := []string{
		"a", "sensors", "my-sensors", "temp_1m", "a.b", "Store42",
		"sensors-1m.rollup", strings.Repeat("x", 64),
	}
	for _, name := range valid {
		if err := ValidateStoreName(name); err != nil {
			t.Errorf("ValidateStoreName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"", ".", "..", "../evil", "a/b", `a\b`, "/abs", ".hidden",
		"-leading", "_leading", "a b", "sensörs", "a\x00b",
		strings.Repeat("x", 65),
	}
	for _, name := range invalid {
		if err := ValidateStoreName(name); err == nil {
			t.Errorf("ValidateStoreName(%q) = nil, want error", name)
		}
	}
}

func TestCreateRejectsTraversalName(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "../escaped-store"
	cfg.Path = filepath.Join(tmpDir, "data")
	if err := os.MkdirAll(cfg.Path, 0755); err != nil {
		t.Fatal(err)
	}

	if _, err := Create(cfg); !errors.Is(err, ErrInvalidStoreName) {
		t.Fatalf("Create with traversal name: got %v, want ErrInvalidStoreName", err)
	}

	// Nothing may have been created outside the data directory
	if _, err := os.Stat(filepath.Join(tmpDir, "escaped-store")); !os.IsNotExist(err) {
		t.Fatal("Create with traversal name created a directory outside the data path")
	}
}

func TestOpenRejectsTraversalName(t *testing.T) {
	tmpDir := t.TempDir()
	for _, name := range []string{"..", "../x", "a/b"} {
		if _, err := Open(tmpDir, name); !errors.Is(err, ErrInvalidStoreName) {
			t.Errorf("Open(%q): got %v, want ErrInvalidStoreName", name, err)
		}
	}
}

func TestDeleteStoreRejectsTraversalName(t *testing.T) {
	tmpDir := t.TempDir()
	base := filepath.Join(tmpDir, "data")
	victim := filepath.Join(tmpDir, "victim")
	for _, dir := range []string{base, victim} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	if err := DeleteStore(base, "../victim"); !errors.Is(err, ErrInvalidStoreName) {
		t.Fatalf("DeleteStore with traversal name: got %v, want ErrInvalidStoreName", err)
	}

	// The directory outside the data path must still exist
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("DeleteStore with traversal name removed a directory outside the data path")
	}
}
