// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinkedKeyFileValidation(t *testing.T) {
	base := t.TempDir()
	// Source and target store dirs must exist for key files to land in them.
	if err := os.MkdirAll(filepath.Join(base, "source"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "target"), 0755); err != nil {
		t.Fatal(err)
	}

	m := NewManager(base)

	// Generate a key on the source.
	srcKey, _, err := m.Generate("source", "initial")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Link the target to the source (no keys of its own).
	if err := m.CreateLinkedKeyFile("target", "source"); err != nil {
		t.Fatalf("CreateLinkedKeyFile: %v", err)
	}

	// The source key validates against the linked target.
	if _, err := m.Validate("target", srcKey); err != nil {
		t.Errorf("source key should validate against linked target: %v", err)
	}

	// A bogus key does not.
	if _, err := m.Validate("target", "tsstore_bogus"); err == nil {
		t.Error("bogus key validated against linked target")
	}

	// Rotation on the source propagates: a NEW source key validates on target,
	// and the old one stops (single source of truth, no drift).
	newKey, _, err := m.Regenerate("source", "rotated")
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if _, err := m.Validate("target", newKey); err != nil {
		t.Errorf("rotated source key should validate against target: %v", err)
	}
	if _, err := m.Validate("target", srcKey); err == nil {
		t.Error("old source key should no longer validate after rotation")
	}
}

func TestLinkedDependents(t *testing.T) {
	base := t.TempDir()
	for _, d := range []string{"source", "t1", "t2", "unrelated"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManager(base)
	if _, _, err := m.Generate("source", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Generate("unrelated", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateLinkedKeyFile("t1", "source"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateLinkedKeyFile("t2", "source"); err != nil {
		t.Fatal(err)
	}

	deps, err := m.LinkedDependents("source")
	if err != nil {
		t.Fatalf("LinkedDependents: %v", err)
	}
	got := map[string]bool{}
	for _, d := range deps {
		got[d] = true
	}
	if !got["t1"] || !got["t2"] {
		t.Errorf("expected t1 and t2 as dependents, got %v", deps)
	}
	if got["unrelated"] {
		t.Error("unrelated store should not be a dependent")
	}
}
