// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tviviano/ts-store/pkg/schema"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sidecar.json")

	// Fresh write
	if err := writeFileAtomic(path, []byte(`{"v":1}`), 0644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != `{"v":1}` {
		t.Fatalf("read back: %q, err %v", got, err)
	}

	// Overwrite replaces content
	if err := writeFileAtomic(path, []byte(`{"v":2}`), 0644); err != nil {
		t.Fatalf("writeFileAtomic overwrite: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != `{"v":2}` {
		t.Fatalf("after overwrite: %q", got)
	}

	// A stale .tmp from a crashed previous write must not break anything
	if err := os.WriteFile(path+".tmp", []byte("torn garbage"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte(`{"v":3}`), 0644); err != nil {
		t.Fatalf("writeFileAtomic with stale tmp: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != `{"v":3}` {
		t.Fatalf("after stale-tmp write: %q", got)
	}

	// No temp file may remain
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "sidecar.json" {
			t.Errorf("leftover file after atomic writes: %s", e.Name())
		}
	}
}

// Regression test for issue #15: schema saves go through the atomic path so
// a crash mid-write can't leave a truncated schema.json (which would make
// openV2 refuse to open the store).
func TestSchemaSaveLeavesNoTempFile(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "atomic-schema"
	cfg.Path = tmpDir
	cfg.NumBlocks = 8
	cfg.DataType = DataTypeSchema

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer s.Close()

	for i := 0; i < 2; i++ { // two saves: create + append a version
		fields := []schema.Field{{Index: 1, Name: "temperature", Type: "float32"}}
		if i == 1 {
			fields = append(fields, schema.Field{Index: 2, Name: "humidity", Type: "float32"})
		}
		if _, err := s.SetSchema(&schema.Schema{Fields: fields}); err != nil {
			t.Fatalf("SetSchema %d: %v", i, err)
		}
	}

	storeDir := filepath.Join(tmpDir, "atomic-schema")
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file in store dir: %s", e.Name())
		}
	}
}
